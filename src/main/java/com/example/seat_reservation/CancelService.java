package com.example.seat_reservation;

import com.example.seat_reservation.dto.CancelResponse;
import com.example.seat_reservation.exception.AlreadyCancelledException;
import com.example.seat_reservation.exception.ReservationNotFoundException;
import io.micrometer.core.instrument.Counter;
import io.micrometer.core.instrument.MeterRegistry;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Isolation;
import org.springframework.transaction.annotation.Transactional;
import org.springframework.transaction.support.TransactionSynchronization;
import org.springframework.transaction.support.TransactionSynchronizationManager;

import java.time.OffsetDateTime;
import java.util.List;

@Service
public class CancelService {

    private static final Logger log = LoggerFactory.getLogger(CancelService.class);

    private final JdbcTemplate jdbc;
    private final Counter cancelledCounter;

    public CancelService(JdbcTemplate jdbc, MeterRegistry registry) {
        this.jdbc = jdbc;
        this.cancelledCounter = Counter.builder("reservations_cancelled_total").register(registry);
    }

    @Transactional(isolation = Isolation.READ_COMMITTED)
    public CancelResponse cancel(long reservationId, String userId) {

        // Acquire reservation lock
        record ResvRow(String showId, String storedUserId, long amountPaise, String status) {}

        List<ResvRow> rows = jdbc.query(
            "SELECT show_id, user_id, amount_paise, status FROM reservations WHERE id = ? FOR UPDATE",
            (rs, i) -> new ResvRow(
                String.valueOf(rs.getLong("show_id")),
                rs.getString("user_id"),
                rs.getLong("amount_paise"),
                rs.getString("status")
            ),
            reservationId
        );

        if (rows.isEmpty() || !userId.equals(rows.getFirst().storedUserId())) {
            throw new ReservationNotFoundException();
        }

        ResvRow resv = rows.getFirst();

        if ("cancelled".equals(resv.status())) {
            throw new AlreadyCancelledException();
        }

        // Seat IDs sorted ascending for consistent lock order, same as reserve flow.
        List<Long> seatIds = jdbc.queryForList(
            "SELECT seat_id FROM reservation_seats WHERE reservation_id = ? ORDER BY seat_id",
            Long.class, reservationId
        );

        int n = seatIds.size();

        // Acquire lock on the counter.
        jdbc.queryForObject(
            "SELECT reserved_count FROM user_seat_limits WHERE user_id = ? AND show_id = ? FOR UPDATE",
            Integer.class, userId, Long.parseLong(resv.showId())
        );

        // Acquire lock on the seats.
        List<String> seatLabels = jdbc.query(
            con -> {
                var ps = con.prepareStatement(
                    "SELECT id, seat_label FROM seats WHERE id = ANY(?) ORDER BY id FOR UPDATE"
                );
                ps.setArray(1, con.createArrayOf("bigint", seatIds.toArray(Long[]::new)));
                return ps;
            },
            (rs, i) -> rs.getString("seat_label")
        );

        // Release only seats still held by THIS reservation (DB-level guard).
        int released = jdbc.update(
            con -> {
                var ps = con.prepareStatement(
                    "UPDATE seats SET status = 'available', reservation_id = NULL " +
                    "WHERE id = ANY(?) AND reservation_id = ?"
                );
                ps.setArray(1, con.createArrayOf("bigint", seatIds.toArray(Long[]::new)));
                ps.setLong(2, reservationId);
                return ps;
            }
        );
        if (released != n) {
            // Rolls back the whole cancel; never frees someone else's seat.
            throw new IllegalStateException(
                "cancel " + reservationId + " released " + released + " of " + n + " seats");
        }

        jdbc.update(
            "UPDATE user_seat_limits SET reserved_count = reserved_count - ? " +
            "WHERE user_id = ? AND show_id = ?",
            n, userId, Long.parseLong(resv.showId())
        );

        OffsetDateTime cancelledAt = jdbc.queryForObject(
            "UPDATE reservations SET status = 'cancelled', cancelled_at = now() " +
            "WHERE id = ? RETURNING cancelled_at",
            (rs, i) -> rs.getObject("cancelled_at", OffsetDateTime.class),
            reservationId
        );

        // Count only once the cancel has actually committed.
        TransactionSynchronizationManager.registerSynchronization(new TransactionSynchronization() {
            @Override public void afterCommit() { cancelledCounter.increment(); }
        });

        log.info("cancel outcome=cancelled user_id={} reservation_id={} show_id={} seats={}", userId, reservationId, resv.showId(), n);
        return new CancelResponse(
            String.valueOf(reservationId),
            resv.showId(),
            userId,
            seatLabels,
            resv.amountPaise(),
            "cancelled",
            cancelledAt.toInstant()
        );
    }
}