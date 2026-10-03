package com.example.seat_reservation;

import com.example.seat_reservation.dto.ShowResponse;
import com.example.seat_reservation.exception.DuplicateSeatException;
import com.example.seat_reservation.exception.ShowNotFoundException;
import org.springframework.context.ApplicationEventPublisher;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Service;
import org.springframework.transaction.annotation.Transactional;

import java.util.Collections;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

@Service
public class ShowService {

    private final JdbcTemplate jdbc;
    private final ApplicationEventPublisher events;
    private final ShowCacheStore cacheStore;

    public ShowService(JdbcTemplate jdbc, ApplicationEventPublisher events, ShowCacheStore cacheStore) {
        this.jdbc = jdbc;
        this.events = events;
        this.cacheStore = cacheStore;
    }

    public record CreatedShow(long showId, ShowCache show) {}

    @Transactional(readOnly = true)
    public ShowResponse getShow(long showId) {
        ShowCache show = cacheStore.get(showId).orElseThrow(() -> new ShowNotFoundException(showId));

        List<ShowResponse.SeatItem> seats = jdbc.query(
            "SELECT seat_label, status FROM seats WHERE show_id = ? ORDER BY id",
            (rs, i) -> new ShowResponse.SeatItem(rs.getString("seat_label"), rs.getString("status")),
            showId
        );

        int available = (int) seats.stream().filter(s -> "available".equals(s.status())).count();
        int held      = (int) seats.stream().filter(s -> "held".equals(s.status())).count();
        int confirmed = (int) seats.stream().filter(s -> "confirmed".equals(s.status())).count();

        return new ShowResponse(
            String.valueOf(showId),
            show.name(), show.venue(), show.pricePaise(), show.perUserLimit(),
            seats.size(), available, held, confirmed, seats
        );
    }

    @Transactional
    public CreatedShow create(String name, String venue, long pricePaise, int perUserLimit, List<String> labels) {

        // Early duplicate check
        if (new HashSet<>(labels).size() != labels.size()) throw new DuplicateSeatException();

        Long showId = jdbc.queryForObject(
            "INSERT INTO shows (name, venue, total_seats, per_user_limit, price_paise) " +
            "VALUES (?, ?, ?, ?, ?) RETURNING id",
            Long.class,
            name, venue, labels.size(), perUserLimit, pricePaise
        );

        Map<String, Long> labelToId = new LinkedHashMap<>();
        try {
            jdbc.query(
                con -> {
                    var ps = con.prepareStatement(
                        "INSERT INTO seats (show_id, seat_label) " +
                        "SELECT ?, unnest(?::text[]) RETURNING id, seat_label"
                    );
                    ps.setLong(1, showId);
                    ps.setArray(2, con.createArrayOf("text", labels.toArray(String[]::new)));
                    return ps;
                },
                rs -> { labelToId.put(rs.getString("seat_label"), rs.getLong("id")); }
            );
        } catch (DuplicateKeyException e) {
            throw new DuplicateSeatException();
        }

        ShowCache show = new ShowCache(name, venue, perUserLimit, pricePaise, Collections.unmodifiableMap(labelToId));

        // Event dispatched by Spring AFTER this transaction commits.
        // ShowCacheStore.onShowCreated picks it up. Rollback = event never fires.
        events.publishEvent(new ShowCreatedEvent(showId, show));

        return new CreatedShow(showId, show);
    }
}
