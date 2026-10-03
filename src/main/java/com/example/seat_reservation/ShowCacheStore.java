package com.example.seat_reservation;

import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.stereotype.Component;
import org.springframework.transaction.event.TransactionPhase;
import org.springframework.transaction.event.TransactionalEventListener;

import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.concurrent.ConcurrentHashMap;

@Component
public class ShowCacheStore {

    private final ConcurrentHashMap<Long, ShowCache> cache = new ConcurrentHashMap<>();
    private final JdbcTemplate jdbc;

    public ShowCacheStore(JdbcTemplate jdbc) {
        this.jdbc = jdbc;
    }

    @TransactionalEventListener(phase = TransactionPhase.AFTER_COMMIT)
    public void onShowCreated(ShowCreatedEvent event) {
        cache.put(event.showId(), event.show());
    }

    // computeIfAbsent guarantees at most one DB load per showId under concurrent misses.
    // Null return means no show exits so no cache needed hence next caller retries the DB (avoids stale-miss entries).
    public Optional<ShowCache> get(long showId) {
        return Optional.ofNullable(cache.computeIfAbsent(showId, this::loadFromDB));
    }

    private ShowCache loadFromDB(Long showId) {
        record ShowRow(String name, String venue, int perUserLimit, long pricePaise) {}

        List<ShowRow> rows = jdbc.query(
            "SELECT name, venue, per_user_limit, price_paise FROM shows WHERE id = ?",
            (rs, n) -> new ShowRow(
                rs.getString("name"),
                rs.getString("venue"),
                rs.getInt("per_user_limit"),
                rs.getLong("price_paise")
            ),
            showId
        );
        if (rows.isEmpty()) return null;
        ShowRow s = rows.getFirst();

        Map<String, Long> labelToId = new LinkedHashMap<>();
        jdbc.query(
            "SELECT id, seat_label FROM seats WHERE show_id = ? ORDER BY id",
            rs -> { labelToId.put(rs.getString("seat_label"), rs.getLong("id")); },
            showId
        );
        
        return new ShowCache(s.name(), s.venue(), s.perUserLimit(), s.pricePaise(), Collections.unmodifiableMap(labelToId));
    }
}
