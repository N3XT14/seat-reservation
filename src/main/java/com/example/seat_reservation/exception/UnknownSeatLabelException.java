package com.example.seat_reservation.exception;

public class UnknownSeatLabelException extends RuntimeException {
    public UnknownSeatLabelException(String label) {
        super("Unknown seat label: " + label);
    }
}
