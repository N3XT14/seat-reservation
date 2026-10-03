package com.example.seat_reservation.dto;

import java.util.List;

public record ReservationResponse(
    String reservationId,
    String showId,
    String userId,
    List<String> seats,
    long amountPaise,
    String status
) {}
