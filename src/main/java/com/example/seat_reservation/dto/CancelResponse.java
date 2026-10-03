package com.example.seat_reservation.dto;

import java.time.Instant;
import java.util.List;

public record CancelResponse(
    String reservationId,
    String showId,
    String userId,
    List<String> seats,
    long amountPaise,
    String status,
    Instant cancelledAt
) {}
