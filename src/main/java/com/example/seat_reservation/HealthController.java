package com.example.seat_reservation;

import com.zaxxer.hikari.HikariConfig;
import com.zaxxer.hikari.HikariDataSource;
import jakarta.annotation.PreDestroy;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.boot.jdbc.autoconfigure.DataSourceProperties;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

import java.sql.Connection;
import java.sql.Statement;

@RestController
public class HealthController {

    private static final Logger log = LoggerFactory.getLogger(HealthController.class);

    private final HikariDataSource readinessPool;

    public HealthController(DataSourceProperties props) {
        HikariConfig c = new HikariConfig();
        c.setPoolName("readiness");
        c.setJdbcUrl(props.determineUrl());
        c.setUsername(props.determineUsername());
        c.setPassword(props.determinePassword());
        c.setMaximumPoolSize(1);
        c.setMinimumIdle(1);
        c.setConnectionTimeout(2000);        // wait at most 2s for the connection
        c.setValidationTimeout(1000);
        c.setInitializationFailTimeout(-1);  // don't block startup if the DB is down
        c.addDataSourceProperty("connectTimeout", "2");  // pgjdbc, seconds: DB gone
        c.addDataSourceProperty("socketTimeout", "3");   // pgjdbc, seconds: DB hung / partition
        this.readinessPool = new HikariDataSource(c);
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
        try (Connection conn = readinessPool.getConnection();
            Statement st = conn.createStatement()) {
            st.setQueryTimeout(2);
            st.execute("SELECT 1");
            return ResponseEntity.ok("ok");
        } catch (Exception e) {
            log.warn("readiness check failed: {}", e.getMessage());
            return ResponseEntity.status(503).body("db unreachable");
        }
    }

    @PreDestroy
    void close() {
        readinessPool.close();
    }
}