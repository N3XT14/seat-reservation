package com.example.seat_reservation;

import io.micrometer.core.instrument.Gauge;
import io.micrometer.core.instrument.MeterRegistry;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;

import java.util.HashSet;
import java.util.List;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicLong;

@Component
public class SeatAvailableMetrics {

    private final JdbcTemplate jdbc;
    private final MeterRegistry registry;
    // key = "showId:status" e.g. "1:available"
    private final ConcurrentHashMap<String, AtomicLong> gauges = new ConcurrentHashMap<>();

    public SeatAvailableMetrics(JdbcTemplate jdbc, MeterRegistry registry) {
        this.jdbc = jdbc;
        this.registry = registry;
    }

    @Scheduled(fixedDelayString = "${metrics.seat-gauge.interval-ms:5000}")
    public void refresh() {
        record ShowStatus(long showId, String status, long cnt) {}

        List<ShowStatus> rows = jdbc.query(
            con -> {
                var ps = con.prepareStatement(
                    "SELECT show_id, status, COUNT(*) AS cnt FROM seats GROUP BY show_id, status"
                );
                ps.setQueryTimeout(2);
                return ps;
            },
            (rs, i) -> new ShowStatus(rs.getLong("show_id"), rs.getString("status"), rs.getLong("cnt"))
        );

        Set<String> seen = new HashSet<>();
        for (ShowStatus row : rows) {
            String key = row.showId() + ":" + row.status();
            seen.add(key);
            gauges.computeIfAbsent(key, k -> {
                AtomicLong val = new AtomicLong();
                Gauge.builder("seats_available", val, AtomicLong::get)
                    .tag("show_id", String.valueOf(row.showId()))
                    .tag("status", row.status())
                    .register(registry);
                return val;
            }).set(row.cnt());
        }

        // Zero out combinations that disappeared (e.g. all seats confirmed — no available row returned).
        gauges.forEach((key, val) -> {
            if (!seen.contains(key)) val.set(0);
        });
    }
}
