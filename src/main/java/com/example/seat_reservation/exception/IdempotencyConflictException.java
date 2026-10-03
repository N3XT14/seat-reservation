package com.example.seat_reservation.exception;

public class IdempotencyConflictException extends RuntimeException {
    public IdempotencyConflictException() {
        super("Same idempotency key used with different request body");
    }
}
