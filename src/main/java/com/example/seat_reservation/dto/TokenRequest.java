package com.example.seat_reservation.dto;

import jakarta.validation.constraints.NotBlank;
import jakarta.validation.constraints.Pattern;
import jakarta.validation.constraints.Size;

public record TokenRequest(
    @NotBlank @Size(max = 64) @Pattern(regexp = "[A-Za-z0-9_.:-]+") String userId,
    @Pattern(regexp = "user|admin") String role   // null -> "user"
) {}