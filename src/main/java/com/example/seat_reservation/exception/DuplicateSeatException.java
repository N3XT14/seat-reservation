package com.example.seat_reservation.exception;

public class DuplicateSeatException extends RuntimeException {
    public DuplicateSeatException() {
        super("Duplicate seat labels");
    }
}
