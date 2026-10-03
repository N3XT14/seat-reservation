package com.example.seat_reservation;

import com.example.seat_reservation.dto.ReservationResponse;
import com.example.seat_reservation.exception.*;
import tools.jackson.databind.ObjectMapper;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Isolation;
import org.springframework.transaction.annotation.Transactional;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.HashSet;
import java.util.HexFormat;
import java.util.List;
import java.util.stream.Collectors;

@Service
public class ReserveService {

    private static final Logger log = LoggerFactory.getLogger(ReserveService.class);

    private final JdbcTemplate jdbc;
    private final ShowCacheStore cacheStore;
    private final ObjectMapper objectMapper;

    public ReserveService(JdbcTemplate jdbc, ShowCacheStore cacheStore, ObjectMapper objectMapper) {
        this.jdbc = jdbc;
        this.cacheStore = cacheStore;
        this.objectMapper = objectMapper;
    }

    private record KeyRow(String hash, Integer responseCode, String responseBody) {}

    @Transactional(isolation = Isolation.READ_COMMITTED)
    public ReservationResponse reserve(long showId, String userId, List<String> seatLabels, String idempotencyKey) {
        
        // Early duplicate reject (check later if input should be sanitized)
        if (new HashSet<>(seatLabels).size() != seatLabels.size()) {
            throw new DuplicateSeatException();
        }

        String requestHash = computeHash(showId, seatLabels);

        // Idempotency gate
        int inserted = jdbc.update(
            "INSERT INTO idempotency_keys (user_id, idempotency_key, request_hash) " +
            "VALUES (?, ?, ?) ON CONFLICT (user_id, idempotency_key) DO NOTHING",
            userId, idempotencyKey, requestHash
        );
        if (inserted == 0) {
            return handleDuplicateKey(userId, idempotencyKey, requestHash);
        }

        // Cache Load
        ShowCache show = cacheStore.get(showId).orElseThrow(() -> new ShowNotFoundException(showId));

        int n = seatLabels.size();
        
        if (n > show.perUserLimit()) {
            throw new PerUserLimitExceededException(show.perUserLimit());
        }

        // seat labels to IDs and asc sort for deterministic lock order.
        List<Long> seatIds = seatLabels.stream()
            .map(label -> {
                Long id = show.labelToId().get(label);
                if (id == null) throw new UnknownSeatLabelException(label);
                return id;
            })
            .sorted()
            .toList();

        // upsert statement: atomically creates or increments the counter and enforces the limit.
        List<Integer> counts = jdbc.query(
            "INSERT INTO user_seat_limits (user_id, show_id, reserved_count) VALUES (?, ?, ?) " +
            "ON CONFLICT (user_id, show_id) DO UPDATE " +
            "  SET reserved_count = user_seat_limits.reserved_count + ? " +
            "  WHERE user_seat_limits.reserved_count + ? <= ? " +
            "RETURNING reserved_count",
            (rs, i) -> rs.getInt("reserved_count"),
            userId, showId, n, n, n, show.perUserLimit()
        );
        if (counts.isEmpty()) {
            throw new PerUserLimitExceededException(show.perUserLimit());
        }

        // Acquire lock
        List<String> statuses = jdbc.query(
            con -> {
                var ps = con.prepareStatement(
                    "SELECT id, status FROM seats WHERE id = ANY(?) ORDER BY id FOR UPDATE"
                );
                ps.setArray(1, con.createArrayOf("bigint", seatIds.toArray(Long[]::new)));
                return ps;
            },
            (rs, i) -> rs.getString("status")
        );
        if (statuses.size() != seatIds.size() || statuses.stream().anyMatch(s -> !"available".equals(s))) {
            throw new SeatUnavailableException();
        }

        long amountPaise = (long) n * show.pricePaise();

        // Insert reservation.
        Long reservationId = jdbc.queryForObject(
            "INSERT INTO reservations (user_id, show_id, amount_paise) VALUES (?, ?, ?) RETURNING id",
            Long.class, userId, showId, amountPaise
        );

        // Link seats to reservation (audit trail).
        jdbc.update(
            con -> {
                var ps = con.prepareStatement(
                    "INSERT INTO reservation_seats (reservation_id, seat_id) " +
                    "SELECT ?, unnest(?::bigint[])"
                );
                ps.setLong(1, reservationId);
                ps.setArray(2, con.createArrayOf("bigint", seatIds.toArray(Long[]::new)));
                return ps;
            }
        );

        // Claim seats.
        jdbc.update(
            con -> {
                var ps = con.prepareStatement(
                    "UPDATE seats SET status = 'confirmed', reservation_id = ? WHERE id = ANY(?)"
                );
                ps.setLong(1, reservationId);
                ps.setArray(2, con.createArrayOf("bigint", seatIds.toArray(Long[]::new)));
                return ps;
            }
        );

        ReservationResponse response = new ReservationResponse(
            String.valueOf(reservationId),
            String.valueOf(showId),
            userId,
            seatLabels,
            amountPaise,
            "confirmed"
        );

        // Store idempotency response so concurrent duplicates can replay it.
        storeIdempotencyResponse(userId, idempotencyKey, 201, response);

        log.info("reserve outcome=confirmed user_id={} show_id={} seats={}", userId, showId, n);
        return response;
    }

    private ReservationResponse handleDuplicateKey(String userId, String idempotencyKey, String requestHash) {
        KeyRow existing = readIdempotencyKey(userId, idempotencyKey);
        if (existing == null) throw new IllegalStateException("Idempotency key vanished unexpectedly");
        if (!existing.hash().equals(requestHash)) throw new IdempotencyConflictException();
        if (existing.responseCode() == null) throw new IllegalStateException("Idempotency key has no stored response");
        ReservationResponse response = objectMapper.readValue(existing.responseBody(), ReservationResponse.class);
        log.info("reserve outcome=replay user_id={} reservation_id={}", userId, response.reservationId());
        return response;
    }

    private KeyRow readIdempotencyKey(String userId, String idempotencyKey) {
        List<KeyRow> rows = jdbc.query(
            "SELECT request_hash, response_code, response_body::text FROM idempotency_keys " +
            "WHERE user_id = ? AND idempotency_key = ?",
            (rs, n) -> new KeyRow(
                rs.getString("request_hash"),
                (Integer) rs.getObject("response_code"),
                rs.getString("response_body")
            ),
            userId, idempotencyKey
        );
        return rows.isEmpty() ? null : rows.getFirst();
    }

    private void storeIdempotencyResponse(String userId, String idempotencyKey, int code, ReservationResponse response) {
        String json = objectMapper.writeValueAsString(response);
        jdbc.update(
            "UPDATE idempotency_keys SET response_code = ?, response_body = ?::jsonb " +
            "WHERE user_id = ? AND idempotency_key = ?",
            code, json, userId, idempotencyKey
        );
    }

    private static String computeHash(long showId, List<String> seatLabels) {
        String input = showId + ":" + seatLabels.stream().sorted().collect(Collectors.joining(","));
        try {
            byte[] hash = MessageDigest.getInstance("SHA-256").digest(input.getBytes(StandardCharsets.UTF_8));
            return HexFormat.of().formatHex(hash);
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("SHA-256 not available", e);
        }
    }
}
