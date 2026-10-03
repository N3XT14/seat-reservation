-- reservations before seats: seats.reservation_id is a FK to this table.
-- The reservation row is inserted before seats are updated

CREATE TABLE shows (
    id              BIGSERIAL   PRIMARY KEY,
    name            TEXT        NOT NULL,
    venue           TEXT,
    total_seats     INT         NOT NULL CHECK (total_seats > 0),
    per_user_limit  INT         NOT NULL DEFAULT 4 CHECK (per_user_limit > 0),
    price_paise     BIGINT      NOT NULL CHECK (price_paise >= 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE reservations (
    id              BIGSERIAL   PRIMARY KEY,
    user_id         TEXT        NOT NULL,
    show_id         BIGINT      NOT NULL REFERENCES shows(id),
    status          TEXT        NOT NULL DEFAULT 'confirmed'
                    CHECK (status IN ('confirmed', 'cancelled')),
    amount_paise    BIGINT      NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    cancelled_at    TIMESTAMPTZ
);

-- 'held' status and hold_expires_at are schema extension points for future TTL holds.
CREATE TABLE seats (
    id              BIGSERIAL   PRIMARY KEY,
    show_id         BIGINT      NOT NULL REFERENCES shows(id),
    seat_label      TEXT        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'available'
                    CHECK (status IN ('available', 'held', 'confirmed')),
    hold_expires_at TIMESTAMPTZ,
    reservation_id  BIGINT      REFERENCES reservations(id),
    UNIQUE (show_id, seat_label)
);

-- Since Other users have their own rows they never block each other.
CREATE TABLE user_seat_limits (
    user_id         TEXT        NOT NULL,
    show_id         BIGINT      NOT NULL REFERENCES shows(id),
    reserved_count  INT         NOT NULL DEFAULT 0 CHECK (reserved_count >= 0),
    PRIMARY KEY (user_id, show_id)
);

CREATE TABLE reservation_seats (
    reservation_id  BIGINT      NOT NULL REFERENCES reservations(id),
    seat_id         BIGINT      NOT NULL REFERENCES seats(id),
    PRIMARY KEY (reservation_id, seat_id)
);

-- Inserted partial at START of reserve transaction, updated with response at END.
-- On rollback the row disappears so a retry is re-evaluated fresh.
-- On concurrent duplicate: second INSERT blocks until first commits, then reads the stored response.
CREATE TABLE idempotency_keys (
    user_id             TEXT        NOT NULL,
    idempotency_key     TEXT        NOT NULL,
    request_hash        TEXT        NOT NULL,
    response_code       INT,
    response_body       JSONB,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, idempotency_key)
);

CREATE INDEX idx_seats_show_id         ON seats(show_id);
-- Postgres does not auto-index FK columns; needed for cancel (WHERE reservation_id = ?).
CREATE INDEX idx_seats_reservation_id  ON seats(reservation_id);
