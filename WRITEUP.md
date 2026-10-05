# WRITEUP

Seat reservation service: Java 25, Spring Boot 4, PostgreSQL 18 (single database, the only source of truth), deployed on AWS ECS behind an NLB. The chronological build record, including mistakes and dead ends, is in [BUILD_LOG.md](BUILD_LOG.md).

## 1. The atomic decision

Each reserve is one Postgres transaction with three steps, always in this order.

**Step 1: Idempotency gate**

```sql
INSERT INTO idempotency_keys (user_id, idempotency_key, request_hash)
VALUES (?, ?, ?)
ON CONFLICT (user_id, idempotency_key) DO NOTHING;
```

0 rows inserted means the key was seen before, so the request takes the replay path (section 2).

**Step 2: Per-user limit**

Each user has one counter row per show: how many seats they currently hold.

```sql
INSERT INTO user_seat_limits (user_id, show_id, reserved_count) VALUES (?, ?, :n)
ON CONFLICT (user_id, show_id) DO UPDATE
  SET reserved_count = user_seat_limits.reserved_count + :n
  WHERE user_seat_limits.reserved_count + :n <= :limit
RETURNING reserved_count;
```

- No row returned means over the limit: `409 per_user_limit`.
- Why not `COUNT(*)`: ten parallel requests would all count 0 and all pass. The row lock makes one user's requests take turns; other users have their own rows and never wait.

**Step 3: Lock and claim the seats**

```sql
SELECT id, status FROM seats WHERE id = ANY(?) ORDER BY id FOR UPDATE;
-- all 'available'? then, in the same transaction:
INSERT INTO reservations (user_id, show_id, amount_paise) VALUES (?, ?, ?) RETURNING id;
INSERT INTO reservation_seats (reservation_id, seat_id) SELECT ?, unnest(?::bigint[]);
UPDATE seats SET status = 'confirmed', reservation_id = ? WHERE id = ANY(?);
```

- Any seat not available: `409 seat_taken`, and the whole request is declined (all-or-nothing; the service never picks which seats to keep for the user).
- A decline here also rolls back step 2's count, so a failed attempt never uses up the user's limit.

### Why it is race-free

The decision is made while holding the seat's row lock, never from an earlier read. When 500 users storm A12, the first transaction locks it and confirms. The other 499 wait on the lock, then read the committed row, see `confirmed`, and get 409. Exactly one 201.

### Why multi-seat requests cannot deadlock

A deadlock needs two transactions grabbing the same rows in opposite order: U1 locks A12 and waits for A13, while U2 locks A13 and waits for A12. That cannot happen here:

- Seats are always locked lowest id first (`ORDER BY id FOR UPDATE`).
- Reserve and cancel both lock the user's counter row before any seat.

Burst phase 5 tests exactly this: many users book overlapping pairs (A1+A2, A2+A3, …) at the same moment. Zero deadlocks, zero 5xx.

### Fast decline

In a hot-seat storm almost every request loses. So before opening a transaction, one quick read without locks asks: *is this a new key, and is any requested seat already taken?* If yes, return 409 straight away.

- **Safe:** it can only say no, never yes. Every yes goes through step 3 under locks.
- **Retries skip it,** because their key has been seen, so they get their replay (section 2).
- **Edge case:** if the owner cancels in the same instant, we may say "taken" just before the seat frees up. Nothing is lost: the seat goes back to available for the next buyer, and this buyer can retry.
- **Why:** the thousands of losers never queue for DB connections or row locks.

### A busy database returns 429, not 5xx

If the DB cannot serve the request (pool timeout, lock timeout, deadlock, serialization failure), the response is `429 busy` with `Retry-After: 1`.

- **Nothing changed:** the transaction rolled back, so retrying is safe, and with the same key it is idempotent.
- **It is overload, not a bug.** 5xx is kept for real bugs, so a 5xx alert always means the code is wrong.
- 503 would be the textbook status, but it is a 5xx. 429 with `Retry-After` tells the client the same thing: back off and retry.
- `db_busy_responses_total` stayed at 0 across 20K bursts, and a reserve with the DB stopped returned 429.

### Identity

The user id comes only from the verified token: an HS256 JWT with the algorithm pinned, signature checked, and `exp` required. A `user_id` in the request body is ignored. Reservations, limits, and idempotency keys are all keyed on the token's user, so a spoofed body field can only act as the token's owner. Burst phase 4 checks this.

## 2. Idempotency

### Where the key lives

Table `idempotency_keys`, unique on `(user_id, idempotency_key)`.

| Column | Holds |
|---|---|
| `user_id`, `idempotency_key` | The key, scoped per user, so two users' keys never collide |
| `request_hash` | SHA-256 of show id + sorted seat labels (`["A13","A12"]` = `["A12","A13"]`) |
| `response_code`, `response_body` | The stored 201, written in the same transaction as the booking |

The `Idempotency-Key` header wins over the body field.

### How exactly-once works

The key row is the first thing the reserve transaction writes (step 1 in section 1).

- **First request:** inserts the key, books the seats, stores the 201, commits.
- **Duplicate arriving at the same time:** waits on the unique index until the first finishes.
  - First committed: read the stored 201 and return the same reservation (`reason="idempotent_replay"`).
  - First rolled back: carry on as a fresh request.
- **Retries skip the fast decline,** because their key has been seen. Otherwise a retry of a successful booking would see its own seat as taken and get 409.

Nothing moves twice.

### Same key, different body

The hash doesn't match, so the response is `409 idempotent_conflict`. The hash includes the show id, so the same key on a different show is a conflict too.

### Declines are not stored

A declined reserve rolls back, and its key row goes with it.

- A retry of a declined request is evaluated again, not answered with the old 409.
- Safe, because a decline changed nothing.
- Useful: if the seat was cancelled in the meantime, the retry can succeed.

## 3. Holds and expiry

### Model: explicit cancel

`POST /reservations/{id}/cancel`. A reserve confirms immediately, so `held` exists in the status model and the invariant but stays 0. A v1 scoping decision; TTL holds are the planned extension.

### How cancel works

One transaction (READ COMMITTED), same lock order as reserve: counter before seats.

```sql
-- 1. lock the reservation; check owner and status
SELECT show_id, user_id, amount_paise, status FROM reservations WHERE id = ? FOR UPDATE;

-- 2. its seats, sorted (plain read, no lock)
SELECT seat_id FROM reservation_seats WHERE reservation_id = ? ORDER BY seat_id;

-- 3. lock the counter, then the seats, lowest id first
SELECT reserved_count FROM user_seat_limits WHERE user_id = ? AND show_id = ? FOR UPDATE;
SELECT id, seat_label FROM seats WHERE id = ANY(?) ORDER BY id FOR UPDATE;

-- 4. free only seats still owned by this reservation (the guard)
UPDATE seats SET status = 'available', reservation_id = NULL
WHERE id = ANY(?) AND reservation_id = ?;

-- 5. give the seats back to the user's limit; mark cancelled
UPDATE user_seat_limits SET reserved_count = reserved_count - ? WHERE user_id = ? AND show_id = ?;
UPDATE reservations SET status = 'cancelled', cancelled_at = now() WHERE id = ? RETURNING cancelled_at;
```

- **Owner only.** Identity comes from the token. Someone else's reservation returns 404, the same as a missing one, so ids can't be probed.
- **Never resurrects a seat.** `AND reservation_id = ?` means a cancel can only free seats that still belong to this reservation.
- **Can't-happen check.** If the update frees fewer seats than the reservation has, the whole cancel rolls back. That would mean the data is already inconsistent, so it surfaces as a 5xx on purpose: it's a real bug, and it pages.
- **Double cancel:** 409.
- **Re-bookable:** a freed seat is plain `available`. Burst phase 6 races cancels against rebooks of the same seats.

### TTL holds later: lazy expiry

`seats.hold_expires_at` already exists.

- Expiry is checked inside the reserve decision itself: an expired hold counts as available, in both the fast decline and the locked step 3.
- No background job is needed for correctness. `GET /shows/{id}` can report expired holds as available so the counts stay right.
- **The tricky part:** claiming an expired hold must also give the seat back to the previous holder's per-user count, in the same transaction. That locks another user's counter row *after* the seats, which breaks the counter-before-seats order, so it needs care.

## 4. Consistency vs availability under a partition

**Consistency.** Selling one seat twice is the failure that matters; a short pause in sales is recoverable.

### App cannot reach the database

Postgres is the only store that decides anything, so the service refuses rather than guesses.

- `/readyz` returns 503. It uses a dedicated one-connection pool, fails in about 2–3 s, and recovers without a restart.
- Reserves return a retryable `429 busy`, never a decision. Verified with the DB stopped: 429 after the 30 s pool timeout, no 5xx.
- No cache or second store can hand out a seat Postgres doesn't know about. Show metadata is cached in memory, but it never changes after creation.

### Buyer cannot reach the service

The booking commits but the response is lost, so the buyer doesn't know if they got the seat.

- The idempotency key solves this: retry with the same key and get the original reservation, or a fresh attempt if nothing committed.
- The burst tool's Recovery step does exactly this for connection errors, and the run still reconciles to the unit.

### Single primary

- One writer, so no split-brain.
- The cost: a DB outage is a sales outage. Multi-AZ failover is the next step (section 7).

### Why no Redis in front

- An extra hop on every request.
- The burst is over in the first few seconds, so there's no warm cache to gain from.
- Adds latency without guaranteeing anything.
- Never the source of truth: Postgres still makes the final call, and two stores that can disagree put the invariant at risk.

## 5. Observability: what pages me at 2am

### Signals

- Prometheus at `/actuator/prometheus`.
- ECS-format JSON logs with `request_id` on every line (`X-Request-Id` propagated or generated).
- CloudWatch for the NLB, ECS, and RDS.

### Page on

| Alert | Why |
|---|---|
| Any 5xx | The design allows none, so even one is a bug |
| `seats{available} + seats{held} + seats{confirmed} != seats_capacity` | Invariant broken: a correctness failure |
| `reservations_confirmed_total − reservations_cancelled_total` ≠ active reservations in the DB | Same; the counter counts reservations, not seats |
| `/readyz` failing, or NLB healthy hosts at 0 | Sales are down |
| `db_busy_responses_total` climbing, or Hikari pending threads stuck at pool max | DB is the bottleneck; users are getting 429s |

**Warn, not page:** p99 regression, task restarts, RDS CPU or connections trending up.

### First response

- **5xx:** take its `X-Request-Id` and pull every log line for that request.
- **Invariant break:** stop sales first by scaling the service to zero (the NLB checks liveness, so failing readiness alone won't stop traffic). A paused sale beats a double-sell. Then compare `seats` against `reservation_seats` for that show.

### Why the metrics can be trusted

- Seat gauges are recomputed from the DB every second, not tracked in memory, so they match the API by construction.
- Counters increment only after commit, so rolled-back work is never counted.

## Evidence

### 20K burst, six phases, live URL

| Check | Result |
|---|---|
| 5xx | 0 |
| Requests accounted for | 20,500 / 20,500 |
| Hot seats | Exactly one 201 each |
| Invariant | Exact, during and after |
| Server counters vs client outcomes | Reconciled |
| p99 latency | ~2.7 s warm, ~7.9 s right after a task restart |
| RDS CPU | ~9% |


### ALB to NLB

- Behind the original ALB, 6 of 9 uncapped 20.5K runs lost ~5–6.5K requests.
- The lost requests never appeared in the ALB access logs; runs sent directly to the task lost none.
- With the NLB (TCP pass-through): 5 of 5 clean, and every run since.
- The limit was connection handling in front of the app, not the database.

## 6. AI usage

**Decided by me:** the stack, and JdbcTemplate over JPA so every statement and lock order is explicit; the global lock order; the counter row for per-user limits; all-or-nothing multi-seat; explicit cancel for v1; no Redis; the fast-decline path; moving from ALB to NLB after I traced the dropped requests; and the auth model for graders.

**Main service:** I designed it and wrote it with AI as coding help: AI produced first passes and boilerplate, and I reviewed every change. Some of the decisions I made while reviewing the AI's output:

- **One statement to claim seats.** Status and `reservation_id` are set in a single `UPDATE`, so both change together in one round trip.
- **Unknown seat label returns 400.** An unknown seat is a client error, not a conflict, so it gets its own exception and status.
- **Duplicate labels are rejected up front.** `["A1","A1"]` is caught in application code before any DB work, giving a clear error instead of relying on a constraint violation.

**Testing and debugging:** I ran the tests and led the debugging; AI helped analyze failures. Examples:

- **Wrong busy exception.** AI wired `CannotGetJdbcConnectionException` as the DB-busy handler. Under load, the logs showed the real exception was `CannotCreateTransactionException` (Spring wraps Hikari's timeout). I spotted it in the logs and directed the fix.
- **Dropped requests behind the ALB.** I noticed requests going missing, compared the client counts with the app's counters and the ALB access logs, and ran bursts directly against the task to isolate the ALB. AI helped me read the logs and numbers; the switch to the NLB was my call.

**Burst tool and scripts:** I wrote the burst tool in Python (asyncio + httpx) first. It topped out at about 76 req/s from my Mac and 126 req/s from an AWS machine: asyncio runs on a single event loop in one thread, so a bigger machine barely helped. I then used AI to port it to Go, whose goroutines spread across all cores; the same machine reached about 600 req/s, enough to actually generate a 20K burst. The deploy, metrics, and log scripts were written the same way, with me specifying and AI helping write.

**Docs:** this write-up and the README were drafted with AI from my notes and code, then edited by me.

## 7. What I'd do next

- **Two or more tasks**, to remove the ~35 s outage when the single task is replaced. This needs metrics fixed first: counters are per task, so scraping through the load balancer would return a different task each time. Prometheus would scrape each task directly and sum across them. Correctness is unaffected, since Postgres decides everything and the seat gauges are read from the DB.
- **RDS Multi-AZ** so a database failure becomes a short failover rather than an outage, with consistency kept because the standby is synchronous.
- **JVM warm-up** before taking traffic. The first burst after a cold start has p99 around 8 s, versus about 2.7 s warm.
- **Fail fast when the DB is down.** Reserves currently wait the full 30 s pool timeout before returning 429. A circuit breaker that trips on readiness failure would answer in milliseconds.
- **TTL holds** using the existing `hold_expires_at` column (section 3).
- **Idempotency key retention:** expire or clean up old keys; today they are kept forever.
- **Validate incoming `X-Request-Id`** (length and charset) before it goes into logs.
- **Outbox publisher:** the `outbox` table exists; add a poller to publish reservation events.
- **Rate limiting and abuse controls** per user and per IP; user tokens are open by design here.
- **Read path scaling** for `GET /shows/{id}` under heavy polling: a read replica for the per-seat view.