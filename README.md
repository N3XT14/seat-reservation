# Seat Reservation Service

A JSON HTTP API that decides, atomically and correctly, who gets each seat when tens of thousands of buyers hit "book" in the same second. Java 25 (Spring Boot, virtual threads), PostgreSQL 18, deployed on AWS ECS behind a Network Load Balancer.

Design details and trade-offs are in [WRITEUP.md](WRITEUP.md).

## Live service

- **Base URL:** http://seat-nlb-a1a61f5739a9984f.elb.us-east-2.amazonaws.com
- **Admin key:** shared privately with the submission (not stored in this repo)

Quick check:

```bash
BASE=http://seat-nlb-a1a61f5739a9984f.elb.us-east-2.amazonaws.com
curl $BASE/healthz
curl $BASE/readyz
```

## Authentication

Identity always comes from the bearer token, never from the request body.

```bash
# User token (open)
curl -X POST $BASE/auth/token -H 'Content-Type: application/json' \
  -d '{"user_id":"alice"}'

# Admin token (requires the admin key)
curl -X POST $BASE/auth/token -H 'Content-Type: application/json' \
  -H "X-Admin-Key: $ADMIN_KEY" \
  -d '{"user_id":"ops","role":"admin"}'
```

Use the returned `token` as `Authorization: Bearer <token>`. Tokens are HS256 JWTs valid for 2 hours. User tokens are open by design (this endpoint stands in for a real identity provider); an admin token without a matching `X-Admin-Key` gets `403`, and if no admin key is configured, admin tokens are never issued.

## API

All requests and responses are JSON. Send `Content-Type: application/json` on every POST. Money is integer paise. `GET /shows/{id}`, health, and metrics need no token; everything else needs `Authorization: Bearer <token>`.

| Method | Path | Who | Purpose |
|---|---|---|---|
| POST | `/auth/token` | any / admin | Issue tokens |
| POST | `/shows` | admin | Create a show with its seats and price |
| GET | `/shows/{id}` | any | Per-seat status and counts |
| POST | `/shows/{id}/reserve` | user | Reserve seats with an idempotency key |
| POST | `/reservations/{id}/cancel` | owner | Release a reservation |
| GET | `/healthz` | any | Liveness |
| GET | `/readyz` | any | Readiness: checks the DB, returns 503 when unreachable |
| GET | `/actuator/prometheus` | any | Prometheus metrics |

### Walkthrough

Needs `curl` and `jq`.

```bash
BASE=http://seat-nlb-a1a61f5739a9984f.elb.us-east-2.amazonaws.com
J='Content-Type: application/json'

# Tokens
ADMIN=$(curl -s -X POST $BASE/auth/token -H "$J" -H "X-Admin-Key: $ADMIN_KEY" \
  -d '{"user_id":"ops","role":"admin"}' | jq -r .token)
ALICE=$(curl -s -X POST $BASE/auth/token -H "$J" -d '{"user_id":"alice"}' | jq -r .token)

# Create a show (venue and per_user_limit are optional; the limit defaults to 4)
SHOW=$(curl -s -X POST $BASE/shows -H "Authorization: Bearer $ADMIN" -H "$J" \
  -d '{"name":"friday-night","seats":["A1","A2","A3"],"price_paise":25000,"per_user_limit":4}' | jq -r .id)

# Reserve (the key can also go in the body as "idempotency_key")
curl -s -X POST $BASE/shows/$SHOW/reserve -H "Authorization: Bearer $ALICE" -H "$J" \
  -H 'Idempotency-Key: k1' -d '{"seats":["A1","A2"]}'

# Show state
curl -s $BASE/shows/$SHOW

# Cancel (owner only; use the reservation_id from the reserve response)
curl -s -X POST $BASE/reservations/<reservation_id>/cancel -H "Authorization: Bearer $ALICE"
```

### Requests and responses

**`POST /auth/token`**: body `{"user_id":"alice"}`, or `{"user_id":"ops","role":"admin"}` with the `X-Admin-Key` header. `user_id` is 1–64 characters of `A-Z a-z 0-9 _ . : -`. Returns `200`:

```json
{"token":"eyJ...","user_id":"alice","role":"user","expires_at":1791193564}
```

`expires_at` is in Unix seconds; tokens last 2 hours.

**`POST /shows`** (admin): returns `201`:

```json
{"id":"1","name":"friday-night","venue":null,"price_paise":25000,"per_user_limit":4,
 "total_seats":3,"available":3,"held":0,"confirmed":0,
 "seats":[{"label":"A1","status":"available"},{"label":"A2","status":"available"},{"label":"A3","status":"available"}]}
```

**`GET /shows/{id}`**: returns `200` with the same shape and live statuses. `available + held + confirmed == total_seats` always holds. A reserve confirms immediately, so `held` is always 0.

**`POST /shows/{id}/reserve`**: body `{"seats":["A1","A2"]}`, with the key either in the `Idempotency-Key` header or as `"idempotency_key"` in the body (the header wins if both are sent). Returns `201`:

```json
{"reservation_id":"1","show_id":"1","user_id":"alice","seats":["A1","A2"],"amount_paise":50000,"status":"confirmed"}
```

Any `user_id` in the body is ignored; identity comes from the token.

Idempotency: keys are scoped per user, so two users can use the same key independently. A retry with the same key and the same seats, in any order, returns `201` with the original response and changes nothing, even if the reservation was cancelled since. The same key with different seats, or on a different show, is `409 idempotent_conflict`.

Multi-seat requests are **all-or-nothing**: if any requested seat is not available, the whole request is declined with `409` and no seat changes state. Seats are locked in ascending id order, so overlapping multi-seat requests cannot deadlock.

**`POST /reservations/{id}/cancel`** (owner): returns `200`, and the seats return to available:

```json
{"reservation_id":"1","show_id":"1","user_id":"alice","seats":["A1","A2"],"amount_paise":50000,
 "status":"cancelled","cancelled_at":"2026-10-05T07:46:05.010988Z"}
```

### Errors

Every error body has the shape `{"error":"<code>","detail":...}`.

| Status | `error` | When |
|---|---|---|
| 400 | `validation_failed` | Missing or invalid fields (`detail` lists them) |
| 400 | `malformed_request` | Body is not valid JSON |
| 400 | `missing_idempotency_key` | No key in the header or body |
| 400 | `duplicate_seat_labels` | The same seat is listed twice |
| 400 | `bad_request` | Unknown seat label, or a non-numeric id in the path |
| 401 | `unauthorized` | Missing, invalid, or expired token. This is also returned for unknown paths and wrong methods when no token is sent, since authentication runs first |
| 403 | `forbidden` | Non-admin creating a show, or an admin token requested without a valid key |
| 404 | `show_not_found` / `reservation_not_found` | Unknown id. Cancelling another user's reservation also returns 404, so ids don't leak |
| 404 | `not_found` | Unknown path (with a valid token) |
| 405 | `method_not_allowed` | Wrong method on a known path (with a valid token) |
| 409 | `seat_taken` | A requested seat is already confirmed |
| 409 | `per_user_limit` | The reserve would exceed the show's `per_user_limit` |
| 409 | `idempotent_conflict` | Same key, different seats or show |
| 409 | `already_cancelled` | The reservation was already cancelled |
| 415 | `unsupported_media_type` | Missing `Content-Type: application/json` |
| 429 | `busy` | The DB is saturated; retry after `Retry-After` (1 second) with the same key |

## Run locally

Requires Docker.

```bash
git clone https://github.com/N3XT14/seat-reservation && cd seat-reservation
docker compose up --build
curl localhost:8080/readyz
```

The local admin key defaults to `local-admin-key` (override with `ADMIN_KEY=... docker compose up`).

## Burst test (one command)

Uses Go if installed, otherwise builds and runs the load generator in Docker. The script raises the file-descriptor limit itself. (`burst-py.sh` and `tools/loadgen-py` are the superseded first version, kept for history.)

**The on-sale stampede (what the spec asks for):** 20K concurrent reserves, 20 hot seats.

```bash
ADMIN_KEY=<key> CONCURRENCY=20000 SEATS=200 HOT=20 \
  ./burst.sh http://seat-nlb-a1a61f5739a9984f.elb.us-east-2.amazonaws.com --client-cap 2000
```

`--client-cap 2000` limits in-flight requests from your machine so a laptop's network stack doesn't become the bottleneck; all 20K are still fired. Drop it to fire everything at once.

> **Client-side limits.** At 20K, the machine and network firing the burst are usually the bottleneck, not the service. From a home connection, my ISP blocked the destination IPs mid-burst, which looks like dropped requests. My 20K evidence runs were fired from a load-generator task inside AWS (same region); the [logs recording](#logs) shows the service under a full 20K-concurrent burst. If a run from your machine reports connection errors, check the client and network before the service: the server-side counters printed at the end show what the service actually received. For example, a 20K run from my home connection showed `WARN … 8 connection errors, all recovered by idempotent retry` with `never reached app: 0` and `lost after app: 8`: the service answered all 20,500, but 8 answers outlived the client timeout over the home link, and retrying them with the same key returned the correct result. The same run from inside AWS had zero errors.

**Quick smoke run** (300 requests, with no options set):

```bash
ADMIN_KEY=<key> ./burst.sh http://seat-nlb-a1a61f5739a9984f.elb.us-east-2.amazonaws.com
```

**Local compose, or a single phase:**

```bash
ADMIN_KEY=local-admin-key ./burst.sh http://localhost:8080
ADMIN_KEY=<key> ./burst.sh <BASE_URL> --phase 2
```

| Variable / flag | Default | Meaning |
|---|---|---|
| `CONCURRENCY` | 300 | Reserve requests fired in phase 1 |
| `SEATS` | 60 | Seats in the fresh show |
| `HOT` | 5 | Hot seats that many users storm at once |
| `--client-cap N` | uncapped | Max in-flight requests from the client |
| `--phase N` | all | Run one phase only |

The run creates a fresh show and executes six phases:

1. **Main burst:** all requests at once, half storming a few hot seats and half spread over the rest; each hot seat must have exactly one `201`.
2. **Per-user limit:** one user fires 10 parallel reserves on a limit-4 show and must end with at most 4 seats.
3. **Idempotency:** same key and body returns the same reservation; same key with different seats gets `409`.
4. **Identity:** a spoofed `user_id` in the body is ignored, and cancelling another user's reservation is refused.
5. **Overlapping multi-seat:** many users request overlapping adjacent pairs at once; all-or-nothing holds with no deadlocks.
6. **Cancel vs rebook:** owners cancel while others race to rebook the same seats; no seat is resurrected or double-sold.

It prints the outcome distribution (confirmed / declined by reason / 5xx), latency percentiles, and the final reconciliation against both the API and server-side Prometheus counters.

`run-burst.sh` is my own tool for running the same load generator as a Fargate task inside AWS; it needs access to my account, so use `burst.sh`.

## Metrics

Scrape `$BASE/actuator/prometheus`, or check the key series directly:

```bash
curl -s $BASE/actuator/prometheus | grep -E '^(reservations_|seats)'
```

Key series:

- `reservations_confirmed_total`: reservations confirmed (incremented after commit)
- `reservations_declined_total{reason=...}`: declines by reason, one of `seat_taken`, `per_user_limit`, `idempotent_replay` (retry answered with the original reservation), `idempotent_conflict` (same key, different seats), `duplicate_seat`, `unknown_seat`, `missing_idempotency_key`
- `seats{status="available|held|confirmed"}`, `seats_available`, and `seats_capacity`: seat state, refreshed every second
- `reservations_cancelled_total`, `db_busy_responses_total`

`./scripts/verify-metrics.sh <BASE_URL>` walks through a scripted sequence and checks that metrics reconcile with API state.

## Logs

Structured JSON logs with a request id on every line (`request_id` field; returned as the `X-Request-Id` response header, and an incoming `X-Request-Id` is reused, otherwise a UUID is generated).

- Recording of the live logs during a 20K burst, including a request-id trace: https://youtu.be/hdH-zY1mXms
- With AWS access: `./scripts/tail-logs.sh` (CloudWatch group `/ecs/seat-reservation`)

## Deploy

Infrastructure on AWS us-east-2: ECS service `seat-svc` (cluster `seat`), RDS PostgreSQL, NLB. Secrets come from SSM Parameter Store. The task definition lives in `infra/seat-svc-taskdef.json`.

```bash
./scripts/release-app.sh        # build and push image from the committed source
./scripts/deploy-app.sh <rev>   # roll the service to that task definition
```
