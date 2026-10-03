package com.example.seat_reservation;

import io.micrometer.core.instrument.Gauge;
import io.micrometer.core.instrument.MeterRegistry;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;

import java.util.HashMap;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicLong;
import java.util.stream.Collectors;

@Component
public class SeatAvailableMetrics {

    private final JdbcTemplate jdbc;
    private final MeterRegistry registry;
    private final ConcurrentHashMap<Long, AtomicLong> gauges = new ConcurrentHashMap<>();

    public SeatAvailableMetrics(JdbcTemplate jdbc, MeterRegistry registry) {
        this.jdbc = jdbc;
        this.registry = registry;
    }

    @Scheduled(fixedDelay = 1000)
    public void refresh() {
        record ShowCount(long showId, long cnt) {}

        Map<Long, Long> counts = jdbc.query(
            "SELECT show_id, count(*) AS cnt FROM seats WHERE status = 'available' GROUP BY show_id",
            (rs, i) -> new ShowCount(rs.getLong("show_id"), rs.getLong("cnt"))
        ).stream().collect(Collectors.toMap(ShowCount::showId, ShowCount::cnt));

        counts.forEach((showId, cnt) ->
            gauges.computeIfAbsent(showId, id -> {
                AtomicLong val = new AtomicLong();
                Gauge.builder("seats_available", val, AtomicLong::get)
                    .tag("show_id", String.valueOf(id))
                    .register(registry);
                return val;
            }).set(cnt)
        );

        // Shows with no available seats won't appear in the query — zero them out.
        gauges.forEach((showId, val) -> {
            if (!counts.containsKey(showId)) val.set(0);
        });
    }
}
