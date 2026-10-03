package com.example.seat_reservation;

// Published by ShowService after the show transaction commits.
// ShowCacheStore listens with @TransactionalEventListener(AFTER_COMMIT) so
// cache.put runs only after commit — a rollback never pollutes the cache.
public record ShowCreatedEvent(long showId, ShowCache show) {}
