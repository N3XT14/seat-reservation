package com.example.seat_reservation;

import com.example.seat_reservation.dto.ReservationResponse;
import com.example.seat_reservation.dto.ReserveRequest;
import com.example.seat_reservation.exception.MissingIdempotencyKeyException;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.validation.Valid;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;

@RestController
public class ReserveController {

    private final ReserveService reserveService;

    public ReserveController(ReserveService reserveService) {
        this.reserveService = reserveService;
    }

    @PostMapping("/shows/{showId}/reserve")
    public ResponseEntity<ReservationResponse> reserve(
            @PathVariable long showId,
            @RequestHeader(value = "Idempotency-Key", required = false) String idempotencyKeyHeader,
            @Valid @RequestBody ReserveRequest req,
            HttpServletRequest request) {

        String userId = (String) request.getAttribute("user_id");

        String idempotencyKey = idempotencyKeyHeader != null ? idempotencyKeyHeader : req.idempotencyKey();
        if (idempotencyKey == null || idempotencyKey.isBlank()) {
            throw new MissingIdempotencyKeyException();
        }

        ReservationResponse response = reserveService.reserve(showId, userId, req.seats(), idempotencyKey);
        return ResponseEntity.status(201).body(response);
    }
}
