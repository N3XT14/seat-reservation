#!/usr/bin/env python3
"""
burst.py — Comprehensive reservation test harness.

Phase 1 — Main burst:  N concurrent reserves, per-seat winner check, 409 reasons,
                        duplicate-key replays mixed in, background GET invariant poll.
Phase 2 — Limit test:  One user fires 10 parallel reserves for 10 seats on a limit-4
                        show. Asserts exactly 4 succeed and the rest are per_user_limit.
Phase 3 — Idempotency: Same key + same body → replay (same reservation_id, no growth).
                        Same key + different seats → 409 idempotency_conflict.
Phase 4 — Identity:    Spoofed user_id in body is ignored. Cancel another user's
                        reservation returns 404.

409 reasons: seat_taken, per_user_limit_exceeded, idempotency_conflict.

Usage:
    JWT_SECRET=<secret> python3 burst.py [BASE_URL] [OPTIONS]
    ./burst.sh                       # sets ulimit, reads env vars, one command

Dependencies:
    pip install httpx pyjwt
"""
import asyncio
import argparse
import os
import random
import time
from collections import Counter, defaultdict

import httpx
import jwt

DEFAULT_BASE = "http://localhost:8080"
JWT_SECRET = os.environ.get("JWT_SECRET", "change-me-in-production-this-is-at-least-32-bytes-long!!")

PASS = "\033[32mPASS\033[0m"
FAIL = "\033[31mFAIL\033[0m"


# ── token factory ────────────────────────────────────────────────────────────

def make_token(user_id: str, role: str = "user") -> str:
    return jwt.encode(
        {"user_id": user_id, "role": role, "exp": int(time.time()) + 7200},
        JWT_SECRET,
        algorithm="HS256",
    )


# ── shared helpers ───────────────────────────────────────────────────────────

async def create_show(client: httpx.AsyncClient, seat_labels: list[str],
                      per_user_limit: int = 4, price: int = 100) -> str:
    r = await client.post(
        "/shows",
        json={"name": "Burst Test", "price_paise": price,
              "per_user_limit": per_user_limit, "seats": seat_labels},
        headers={"Authorization": f"Bearer {make_token('burst-admin', 'admin')}"},
    )
    if r.status_code != 201:
        raise RuntimeError(f"Create show failed {r.status_code}: {r.text}")
    return r.json()["id"]


async def get_show(client: httpx.AsyncClient, show_id: str) -> dict:
    r = await client.get(f"/shows/{show_id}")
    r.raise_for_status()
    return r.json()


def assert_invariant(data: dict, label: str = "") -> bool:
    av, held, conf, total = data["available"], data["held"], data["confirmed"], data["total_seats"]
    ok = av + held + conf == total
    tag = PASS if ok else FAIL
    prefix = f"[{label}] " if label else ""
    print(f"  {prefix}invariant: available({av})+held({held})+confirmed({conf})={av+held+conf}"
          f"  total_seats={total}  {tag}")
    return ok


# ── single reserve call ──────────────────────────────────────────────────────

class ReserveResult:
    __slots__ = ("seat", "user_id", "status", "reason", "reservation_id")

    def __init__(self, seat, user_id, status, reason="", reservation_id=""):
        self.seat = seat
        self.user_id = user_id
        self.status = status          # int HTTP status or "ERR"
        self.reason = reason          # error field from JSON on 4xx
        self.reservation_id = reservation_id


async def reserve_one(client: httpx.AsyncClient, show_id: str, user_id: str,
                      seats: list[str], ikey: str, token: str,
                      sem: asyncio.Semaphore) -> ReserveResult:
    seat_key = seats[0] if len(seats) == 1 else ",".join(sorted(seats))
    async with sem:
        try:
            r = await client.post(
                f"/shows/{show_id}/reserve",
                json={"seats": seats},
                headers={"Authorization": f"Bearer {token}",
                         "Idempotency-Key": ikey},
            )
            body = {}
            ct = r.headers.get("content-type", "")
            if "application/json" in ct:
                body = r.json()
            reason = body.get("error", "") if r.status_code != 201 else ""
            resv_id = body.get("reservation_id", "") if r.status_code == 201 else ""
            return ReserveResult(seat_key, user_id, r.status_code, reason, resv_id)
        except Exception as exc:
            return ReserveResult(seat_key, user_id, "ERR", f"{type(exc).__name__}: {exc!r}")


# ── phase 1: main burst ──────────────────────────────────────────────────────

async def _invariant_poller(client: httpx.AsyncClient, show_id: str,
                             stop: asyncio.Event, violations: list[str],
                             interval: float = 0.2) -> None:
    """Background task: poll GET /shows/{id} and record any invariant failures."""
    while not stop.is_set():
        try:
            data = await get_show(client, show_id)
            av, held, conf, total = (data["available"], data["held"],
                                     data["confirmed"], data["total_seats"])
            if av + held + conf != total:
                violations.append(
                    f"av={av} held={held} conf={conf} sum={av+held+conf} total={total}")
        except Exception as exc:
            violations.append(f"GET error: {type(exc).__name__}: {exc!r}")
        await asyncio.sleep(interval)


async def phase1_burst(client: httpx.AsyncClient, args) -> bool:
    print("\n══════════════ Phase 1: Main burst ══════════════")

    n_hot   = args.hot
    n_total = args.seats
    n_req   = args.concurrency

    all_labels  = [f"S{i:04d}" for i in range(1, n_total + 1)]
    hot_labels  = all_labels[:n_hot]
    cold_labels = all_labels[n_hot:]

    show_id = await create_show(client, all_labels)
    print(f"Show {show_id} | {n_total} seats ({n_hot} hot, {len(cold_labels)} cold)")

    # Pre-mint all tokens before the timed section.
    half = n_req // 2
    tasks_meta: list[tuple[str, list[str], str, str]] = []  # (user_id, seats, ikey, token)

    for i in range(half):
        uid = f"hot-u{i}"
        tasks_meta.append((uid, [random.choice(hot_labels)],
                            f"h{i}-{time.time_ns()}", make_token(uid)))

    cold_ikeys: list[str] = []
    cold_seats: list[str] = []
    cold_users: list[str] = []
    for i in range(n_req - half):
        uid  = f"cold-u{i}"
        ikey = f"c{i}-{time.time_ns()}"
        seat = cold_labels[i % len(cold_labels)]
        cold_ikeys.append(ikey)
        cold_seats.append(seat)
        cold_users.append(uid)
        tasks_meta.append((uid, [seat], ikey, make_token(uid)))

    # Mix in exact duplicate-key replays (~5% of cold requests).
    n_dups = max(1, len(cold_ikeys) // 20)
    for i in range(n_dups):
        uid  = cold_users[i]
        tasks_meta.append((uid, [cold_seats[i]], cold_ikeys[i], make_token(uid)))

    random.shuffle(tasks_meta)

    cap = args.client_cap if args.client_cap else n_req
    sem = asyncio.Semaphore(cap)

    # Start background invariant poller.
    poll_violations: list[str] = []
    stop_event = asyncio.Event()
    poller = asyncio.create_task(
        _invariant_poller(client, show_id, stop_event, poll_violations))

    coros = [
        reserve_one(client, show_id, uid, seats, ikey, tok, sem)
        for uid, seats, ikey, tok in tasks_meta
    ]

    print(f"Firing {len(coros)} requests ({n_dups} duplicate-key replays mixed in) …")
    t0 = time.perf_counter()
    results: list[ReserveResult] = await asyncio.gather(*coros)
    elapsed = time.perf_counter() - t0

    stop_event.set()
    await poller

    # ── tally ──
    status_counts: Counter = Counter()
    reason_counts: Counter = Counter()
    seat_winners: dict[str, list[ReserveResult]] = defaultdict(list)
    err_samples: list[str] = []

    for res in results:
        status_counts[res.status] += 1
        if res.status == 409:
            reason_counts[res.reason or "unknown"] += 1
        if res.status == 201:
            seat_winners[res.seat].append(res)
        if res.status == "ERR" and len(err_samples) < 5:
            err_samples.append(res.reason)

    print(f"\nCompleted in {elapsed:.2f}s  ({len(coros) / elapsed:.0f} req/s)")
    print(f"  201 confirmed : {status_counts[201]}")
    print(f"  409 declined  : {status_counts[409]}")
    for reason, n in reason_counts.most_common():
        print(f"    ↳ {reason}: {n}")
    for code in sorted(c for c in status_counts if c not in (201, 409)):
        print(f"  {code:<16}: {status_counts[code]}")
    if err_samples:
        print(f"  ERR samples:")
        for e in err_samples:
            print(f"    {e}")

    ok = True

    # Per-seat winner: a seat with multiple 201s is only a bug if the
    # reservation_ids differ — same id means idempotency replays (correct).
    multi = {
        s: ws for s, ws in seat_winners.items()
        if len({r.reservation_id for r in ws}) > 1
    }
    replay_seats = {s for s, ws in seat_winners.items() if len(ws) > 1} - multi.keys()
    n_replay_201s = sum(len(ws) - 1 for s, ws in seat_winners.items() if s in replay_seats)
    if multi:
        print(f"\n  {FAIL}  {len(multi)} seat(s) double-booked (different reservation_ids): "
              f"{list(multi)[:3]}")
        ok = False
    else:
        replay_note = f"  ({n_replay_201s} replay 201s from idempotency, correct)" if n_replay_201s else ""
        print(f"\n  {PASS}  per-seat winner: no seat double-booked{replay_note}")

    # confirmed from GET must equal unique reservations (replays don't create new rows).
    show_data = await get_show(client, show_id)
    unique_confirmed = len({r.reservation_id for ws in seat_winners.values() for r in ws})
    if show_data["confirmed"] != unique_confirmed:
        print(f"  {FAIL}  GET confirmed={show_data['confirmed']} "
              f"!= unique reservations={unique_confirmed}")
        ok = False
    else:
        print(f"  {PASS}  GET confirmed={show_data['confirmed']} matches unique reservations")

    if not assert_invariant(show_data):
        ok = False

    n5xx = sum(status_counts[c] for c in status_counts
               if isinstance(c, int) and c >= 500)
    n_err = status_counts.get("ERR", 0)
    if n5xx or n_err:
        print(f"  {FAIL}  {n5xx} 5xx responses, {n_err} connection errors")
        ok = False
    else:
        print(f"  {PASS}  zero 5xx / connection errors")

    if poll_violations:
        print(f"  {FAIL}  {len(poll_violations)} invariant violation(s) during burst:")
        for v in poll_violations[:3]:
            print(f"    {v}")
        ok = False
    else:
        print(f"  {PASS}  no invariant violations during burst polling")

    # Final reconciliation table
    print(f"\n  Reconciliation:")
    print(f"    total_seats : {show_data['total_seats']}")
    print(f"    confirmed   : {show_data['confirmed']}")
    print(f"    available   : {show_data['available']}")
    print(f"    held        : {show_data['held']}")
    chk = show_data['available'] + show_data['held'] + show_data['confirmed']
    tag = PASS if chk == show_data['total_seats'] else FAIL
    print(f"    sum check   : {show_data['available']}+{show_data['held']}+{show_data['confirmed']}={chk} == {show_data['total_seats']}  {tag}")

    return ok


# ── phase 2: per-user limit ──────────────────────────────────────────────────

async def phase2_limit(client: httpx.AsyncClient, args) -> bool:
    print("\n══════════════ Phase 2: Per-user limit ══════════════")

    limit   = 4
    n_seats = 10
    labels  = [f"L{i:03d}" for i in range(1, n_seats + 1)]
    show_id = await create_show(client, labels, per_user_limit=limit)
    print(f"Show {show_id} | {n_seats} seats, per_user_limit={limit}")
    print(f"Firing {n_seats} parallel single-seat reserves for the same user …")

    uid = "limit-test-user"
    tok = make_token(uid)
    sem = asyncio.Semaphore(n_seats)

    coros = [
        reserve_one(client, show_id, uid, [labels[i]],
                    f"lim-{i}-{time.time_ns()}", tok, sem)
        for i in range(n_seats)
    ]
    results: list[ReserveResult] = await asyncio.gather(*coros)

    ok_count    = sum(1 for r in results if r.status == 201)
    limit_hits  = sum(1 for r in results if r.reason == "per_user_limit")
    unexpected  = [r for r in results if r.status not in (201, 409)]

    print(f"  201 confirmed           : {ok_count}  (expected {limit})")
    print(f"  409 per_user_limit      : {limit_hits}  (expected {n_seats - limit})")

    ok = True
    if ok_count != limit:
        print(f"  {FAIL}  expected {limit} confirmed, got {ok_count}")
        ok = False
    else:
        print(f"  {PASS}  exactly {limit} seats confirmed")

    if limit_hits != n_seats - limit:
        print(f"  {FAIL}  expected {n_seats - limit} per_user_limit declines, got {limit_hits}")
        ok = False
    else:
        print(f"  {PASS}  remaining {n_seats - limit} correctly declined as per_user_limit")

    if unexpected:
        print(f"  {FAIL}  unexpected responses: {[(r.status, r.reason) for r in unexpected]}")
        ok = False

    return ok


# ── phase 3: idempotency ─────────────────────────────────────────────────────

async def phase3_idempotency(client: httpx.AsyncClient, args) -> bool:
    print("\n══════════════ Phase 3: Idempotency ══════════════")

    labels  = ["I001", "I002"]
    show_id = await create_show(client, labels)
    uid     = "idem-user"
    tok     = make_token(uid)
    ikey    = f"idem-key-{time.time_ns()}"
    sem     = asyncio.Semaphore(10)
    ok      = True

    # First request.
    r1 = await reserve_one(client, show_id, uid, ["I001"], ikey, tok, sem)
    if r1.status != 201:
        print(f"  {FAIL}  first reserve: expected 201, got {r1.status} {r1.reason}")
        return False
    print(f"  First reserve → 201, reservation_id={r1.reservation_id}")

    # Replay: same key, same seat → must return same reservation_id.
    r2 = await reserve_one(client, show_id, uid, ["I001"], ikey, tok, sem)
    if r2.status != 201:
        print(f"  {FAIL}  replay: expected 201, got {r2.status} {r2.reason}")
        ok = False
    elif r2.reservation_id != r1.reservation_id:
        print(f"  {FAIL}  replay: reservation_id changed "
              f"{r1.reservation_id} → {r2.reservation_id}")
        ok = False
    else:
        print(f"  {PASS}  replay returns same reservation_id ({r1.reservation_id})")

    # confirmed must not grow.
    data = await get_show(client, show_id)
    if data["confirmed"] != 1:
        print(f"  {FAIL}  replay caused extra confirmation: confirmed={data['confirmed']}")
        ok = False
    else:
        print(f"  {PASS}  confirmed=1 after replay (no duplicate seat claimed)")

    # Conflict: same key, different seat → 409 idempotency_conflict.
    r3 = await reserve_one(client, show_id, uid, ["I002"], ikey, tok, sem)
    if r3.status == 409 and r3.reason == "idempotent_conflict":
        print(f"  {PASS}  same key + different seat → 409 idempotent_conflict")
    else:
        print(f"  {FAIL}  same key + different seat: expected 409 idempotent_conflict, "
              f"got {r3.status} {r3.reason!r}")
        ok = False

    return ok


# ── phase 4: identity ────────────────────────────────────────────────────────

async def phase4_identity(client: httpx.AsyncClient, args) -> bool:
    print("\n══════════════ Phase 4: Identity ══════════════")

    labels  = ["ID001", "ID002"]
    show_id = await create_show(client, labels)
    ok      = True

    # Spoofed user_id in body — server must use token's user_id.
    real_uid = "real-user"
    tok = make_token(real_uid)
    r = await client.post(
        f"/shows/{show_id}/reserve",
        json={"seats": ["ID001"], "user_id": "someone-else"},
        headers={"Authorization": f"Bearer {tok}",
                 "Idempotency-Key": f"id-spoof-{time.time_ns()}"},
    )
    if r.status_code != 201:
        print(f"  {FAIL}  spoofed body: expected 201, got {r.status_code} {r.text}")
        ok = False
    else:
        resp_uid = r.json().get("user_id")
        if resp_uid == real_uid:
            print(f"  {PASS}  spoofed body user_id ignored; response user_id = token's ({real_uid})")
        else:
            print(f"  {FAIL}  response user_id={resp_uid!r}, expected {real_uid!r}")
            ok = False

    reservation_id = r.json().get("reservation_id") if r.status_code == 201 else None

    # Cancel by a different user → 404 (no reservation ID leak).
    if reservation_id:
        other_tok = make_token("other-user")
        cr = await client.post(
            f"/reservations/{reservation_id}/cancel",
            headers={"Authorization": f"Bearer {other_tok}"},
        )
        if cr.status_code == 404:
            print(f"  {PASS}  cancel by wrong user → 404 (no reservation ID leak)")
        else:
            print(f"  {FAIL}  cancel by wrong user: expected 404, got {cr.status_code}")
            ok = False
    else:
        print(f"  SKIP  no reservation_id to test cancel ownership (earlier step failed)")

    return ok


# ── entry point ──────────────────────────────────────────────────────────────

async def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    ap.add_argument("base_url", nargs="?", default=DEFAULT_BASE)
    ap.add_argument("--concurrency", type=int, default=300,
                    help="Concurrent requests in phase 1 (default: 300)")
    ap.add_argument("--seats", type=int, default=60,
                    help="Total seats in the phase-1 show (default: 60)")
    ap.add_argument("--hot", type=int, default=5,
                    help="Number of contested hot seats (default: 5)")
    ap.add_argument("--phase", type=int, choices=[1, 2, 3, 4],
                    help="Run only one phase (default: run all)")
    ap.add_argument("--client-cap", type=int, default=0,
                    help="Cap in-flight requests client-side (default: no cap).")
    args = ap.parse_args()

    if args.hot >= args.seats:
        ap.error("--hot must be less than --seats")

    conn_cap = args.client_cap + 10 if args.client_cap else args.concurrency + 10
    limits = httpx.Limits(max_connections=conn_cap, max_keepalive_connections=conn_cap)

    phases = {1: phase1_burst, 2: phase2_limit,
              3: phase3_idempotency, 4: phase4_identity}
    to_run = [args.phase] if args.phase else [1, 2, 3, 4]

    phase_results: dict[int, bool] = {}
    async with httpx.AsyncClient(base_url=args.base_url, timeout=30.0, limits=limits) as client:
        for p in to_run:
            phase_results[p] = await phases[p](client, args)

    labels = {1: "Main burst", 2: "Per-user limit",
              3: "Idempotency", 4: "Identity"}
    print("\n══════════════ Summary ══════════════")
    all_ok = True
    for p, ok in phase_results.items():
        tag = PASS if ok else FAIL
        print(f"  Phase {p}  {labels[p]:<20}  {tag}")
        if not ok:
            all_ok = False

    return 0 if all_ok else 1


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
