package com.example.seat_reservation.exception;

public class SeatUnavailableException extends RuntimeException {
    public SeatUnavailableException() {
        super("One or more seats are no longer available");
    }
}
