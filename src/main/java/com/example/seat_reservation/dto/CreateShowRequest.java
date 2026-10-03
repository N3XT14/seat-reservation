package com.example.seat_reservation.dto;

import jakarta.validation.constraints.NotBlank;
import jakarta.validation.constraints.NotEmpty;
import jakarta.validation.constraints.NotNull;
import jakarta.validation.constraints.Positive;

import java.util.List;

public record CreateShowRequest(
    @NotBlank           String name,
                        String venue,           // optional
    @NotNull @Positive  Long pricePaise,
    @Positive           Integer perUserLimit,   // optional, defaults to 4
    @NotEmpty           List<@NotBlank String> seats
) {}
