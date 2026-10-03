package com.example.seat_reservation.exception;

public class MissingIdempotencyKeyException extends RuntimeException {
    public MissingIdempotencyKeyException() {
        super("Idempotency-Key header or body field is required");
    }
}
