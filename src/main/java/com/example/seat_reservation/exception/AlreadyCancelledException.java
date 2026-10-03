package com.example.seat_reservation.exception;

public class AlreadyCancelledException extends RuntimeException {
    public AlreadyCancelledException() {
        super("Reservation is already cancelled");
    }
}
