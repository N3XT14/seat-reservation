package com.example.seat_reservation.exception;

public class ShowNotFoundException extends RuntimeException {
    public ShowNotFoundException(long showId) {
        super("Show not found: " + showId);
    }
}
