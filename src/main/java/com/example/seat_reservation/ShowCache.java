package com.example.seat_reservation;

import java.util.Map;

// Immutable snapshot of a show's static metadata, stored in ShowCacheStore.
// Never changes after creation, so no locking needed on reads.
public record ShowCache(
    String name,
    String venue,
    int perUserLimit,
    long pricePaise,
    Map<String, Long> labelToId   // seat_label → seat.id, unmodifiable
) {}
