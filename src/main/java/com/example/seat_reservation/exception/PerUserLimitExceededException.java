package com.example.seat_reservation.exception;

public class PerUserLimitExceededException extends RuntimeException {

    @Override
    public Throwable fillInStackTrace() { return this; }
    
    public PerUserLimitExceededException(int limit) {
        super("Per-user seat limit of " + limit + " would be exceeded");
    }
}
