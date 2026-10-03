package com.example.seat_reservation.exception;

public class ForbiddenException extends RuntimeException {
    public ForbiddenException() {
        super("Admin role required");
    }
}
