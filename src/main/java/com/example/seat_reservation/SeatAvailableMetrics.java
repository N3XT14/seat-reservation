package com.example.seat_reservation;

import io.micrometer.core.instrument.Gauge;
import io.micrometer.core.instrument.MeterRegistry;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;
import org.springframework.transaction.event.TransactionalEventListener;

import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicLong;

@Component
public class SeatAvailableMetrics {

    private static final List<String> STATUSES = List.of("available", "held", "confirmed");

    private final JdbcTemplate jdbc;
    private final MeterRegistry registry;
    private final ConcurrentHashMap<Long, ShowGauges> shows = new ConcurrentHashMap<>();

    private record ShowGauges(AtomicLong total, Map<String, AtomicLong> byStatus) {}

    public SeatAvailableMetrics(JdbcTemplate jdbc, MeterRegistry registry) {
        this.jdbc = jdbc;
        this.registry = registry;
    }

    // New show: series exist immediately with available = total.
    @TransactionalEventListener
    public void onShowCreated(ShowCreatedEvent e) {
        long total = e.show().labelToId().size();
        ShowGauges g = gaugesFor(e.showId());
        g.total().set(total);
        g.byStatus().get("available").set(total);
    }

    @Scheduled(fixedDelayString = "${metrics.seat-gauge.interval-ms:1000}")
    public void refresh() {
        Map<Long, Long> totals = new HashMap<>();
        jdbc.query(con -> {
            var ps = con.prepareStatement("SELECT id, total_seats FROM shows");
            ps.setQueryTimeout(2);
            return ps;
        }, rs -> { totals.put(rs.getLong("id"), rs.getLong("total_seats")); });

        Map<Long, Map<String, Long>> counts = new HashMap<>();
        jdbc.query(con -> {
            var ps = con.prepareStatement(
                "SELECT show_id, status, COUNT(*) AS cnt FROM seats GROUP BY show_id, status");
            ps.setQueryTimeout(2);
            return ps;
        }, rs -> {
            counts.computeIfAbsent(rs.getLong("show_id"), k -> new HashMap<>())
                  .put(rs.getString("status"), rs.getLong("cnt"));
        });

        // Driven by shows, so every status is set every time (0 when absent).
        totals.forEach((showId, total) -> {
            ShowGauges g = gaugesFor(showId);
            g.total().set(total);
            Map<String, Long> c = counts.getOrDefault(showId, Map.of());
            for (String s : STATUSES) g.byStatus().get(s).set(c.getOrDefault(s, 0L));
        });
    }

    private ShowGauges gaugesFor(long showId) {
        return shows.computeIfAbsent(showId, id -> {
            String sid = String.valueOf(id);
            AtomicLong total = new AtomicLong();
            Gauge.builder("seats_capacity", total, AtomicLong::get)
                .tag("show_id", sid).register(registry);

            Map<String, AtomicLong> byStatus = new HashMap<>();
            for (String s : STATUSES) {
                AtomicLong v = new AtomicLong();
                byStatus.put(s, v);
                Gauge.builder("seats", v, AtomicLong::get)
                    .tag("show_id", sid).tag("status", s).register(registry);
            }
            
            Gauge.builder("seats_available", byStatus.get("available"), AtomicLong::get)
                .tag("show_id", sid).register(registry);
            return new ShowGauges(total, Map.copyOf(byStatus));
        });
    }
}