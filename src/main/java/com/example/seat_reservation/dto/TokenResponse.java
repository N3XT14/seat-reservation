package com.example.seat_reservation.dto;

public record TokenResponse(String token, String userId, String role, long expiresAt) {}