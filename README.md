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

| Method | Path | Who | Purpose |
|---|---|---|---|
| POST | `/shows` | admin | Create a show with its seats and price (integer paise) |
| GET | `/shows/{id}` | any | Per-seat status and counts (available + held + confirmed == total) |
| POST | `/shows/{id}/reserve` | user | Reserve seats with an idempotency key |
| POST | `/reservations/{id}/cancel` | owner | Release a reservation |
| POST | `/auth/token` | any / admin | Issue tokens |
| GET | `/healthz` | any | Liveness |
| GET | `/readyz` | any | Readiness: checks the DB, returns 503 when unreachable |
| GET | `/actuator/prometheus` | any | Prometheus metrics |

Example reserve:

```bash
curl -X POST $BASE/shows/$SHOW/reserve \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: 7f3c...' \
  -d '{"seats":["A12"]}'
```

Outcomes: `201` confirmed; `409` seat taken, per-user limit, or same key with different seats; a retry with the same key and body returns the original reservation.

Multi-seat requests are **all-or-nothing**: if any requested seat is not available, the whole request is declined with `409` and no seat changes state. Seats are locked in ascending id order, so overlapping multi-seat requests cannot deadlock.

A successful reserve confirms the seats immediately; `POST /reservations/{id}/cancel` returns them to available.

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
