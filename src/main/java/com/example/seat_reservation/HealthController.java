package com.example.seat_reservation;

import org.springframework.http.ResponseEntity;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

@RestController
public class HealthController {

    private final JdbcTemplate jdbc;

    public HealthController(JdbcTemplate jdbc) {
        this.jdbc = jdbc;
    }

    // Liveness: is the process up? Always 200 if we reach this line.
    @GetMapping("/healthz")
    public ResponseEntity<String> liveness() {
        return ResponseEntity.ok("ok");
    }

    // Readiness: can we reach the DB? Returns 503 if not (fails closed).
    // Not used as the LB health check: on ECS a failing LB check restarts the
    // task, so a DB outage would cause restart loops. The LB checks /healthz.
    @GetMapping("/readyz")
    public ResponseEntity<String> readiness() {
        try {
            jdbc.queryForObject("SELECT 1", Integer.class);
            return ResponseEntity.ok("ok");
        } catch (Exception e) {
            return ResponseEntity.status(503).body("db unreachable");
        }
    }
}
