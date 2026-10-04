package com.example.seat_reservation.exception;

public class SeatUnavailableException extends RuntimeException {

    @Override
    public Throwable fillInStackTrace() { return this; }

    public SeatUnavailableException() {
        super("One or more seats are no longer available");
    }
}
