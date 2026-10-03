package com.example.seat_reservation.dto;

import jakarta.validation.constraints.NotBlank;
import jakarta.validation.constraints.NotEmpty;

import java.util.List;

public record ReserveRequest(
    @NotEmpty List<@NotBlank String> seats,
    String idempotencyKey  // nullable here; may arrive via Idempotency-Key header instead
) {}
