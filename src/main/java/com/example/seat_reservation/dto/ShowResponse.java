package com.example.seat_reservation.dto;

import java.util.List;

public record ShowResponse(
    String id,
    String name,
    String venue,
    long pricePaise,
    int perUserLimit,
    int totalSeats,
    int available,
    int held,
    int confirmed,
    List<SeatItem> seats
) {
    public record SeatItem(String label, String status) {}
}
