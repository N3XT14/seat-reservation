// Command burst is the seat-reservation load generator and correctness harness.
//
// Phase 1 — Main burst:  N reserves (half on a few hot seats, half spread over cold
//
//	seats, ~5% duplicate-key replays), per-seat winner check,
//	409 reasons, latency, background GET invariant poll,
//	server-side reconciliation, idempotent recovery of connection errors.
//
// Phase 2 — Limit test:  one user fires 10 parallel reserves on a limit-4 show;
//
//	exactly 4 succeed, the rest are per_user_limit.
//
// Phase 3 — Idempotency: same key + same body → same reservation_id, no growth;
//
//	same key + different seats → 409 idempotent_conflict.
//
// Phase 4 — Identity:    spoofed user_id in the body is ignored; cancelling another
//
//	user's reservation returns 404.
//
// Usage:
//
//	ADMIN_KEY=... burst [BASE_URL] [flags]
//	burst http://host --concurrency 20000 --seats 200 --hot 20 --client-cap 2000
//
// Tokens come from the service's POST /auth/token; ADMIN_KEY is needed to create shows.
// All user tokens are minted before the timed burst, so they don't count toward it.
// No third-party dependencies.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// phase1ConnErrs is surfaced in the summary so connection errors aren't hidden
// behind a PASS.
var phase1ConnErrs int

var (
	pass = "\033[32mPASS\033[0m"
	fail = "\033[31mFAIL\033[0m"
)

type config struct {
	baseURL   string
	requests  int
	seats     int
	hot       int
	phase     int
	clientCap int
	timeout   time.Duration
	adminKey  string
}

// ── HTTP helpers ────────────────────────────────────────────────────────────

type api struct {
	base       string
	client     *http.Client
	adminKey   string
	adminToken string
}

type response struct {
	status int
	body   map[string]any
	raw    []byte
	err    error
	dur    time.Duration
}

func (a *api) do(ctx context.Context, method, path string, body any, headers map[string]string) response {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return response{err: err}
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rdr)
	if err != nil {
		return response{err: err}
	}
	// Go's Transport silently re-sends a request whose reused connection dies
	// mid-flight if it carries an Idempotency-Key header and a rewindable body.
	// That hides errors and makes the server process some requests twice, so turn
	// it off: every attempt the harness makes is one it can see and count.
	req.GetBody = nil
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	r, err := a.client.Do(req)
	if err != nil {
		return response{err: err, dur: time.Since(start)}
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body) // read fully so the connection is reused
	out := response{status: r.StatusCode, raw: raw, err: err, dur: time.Since(start)}
	if err == nil && strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var m map[string]any
		if dec.Decode(&m) == nil {
			out.body = m
		}
	}
	return out
}

func str(m map[string]any, k string) string {
	if m == nil || m[k] == nil {
		return ""
	}
	return fmt.Sprint(m[k])
}

func num(m map[string]any, k string) (int, bool) {
	n, ok := m[k].(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return int(i), err == nil
}

func short(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ── tokens
func (a *api) token(ctx context.Context, userID, role string) (string, error) {
	headers := map[string]string{}
	if role == "admin" {
		headers["X-Admin-Key"] = a.adminKey
	}
	var r response
	for attempt := 0; attempt < 3; attempt++ {
		r = a.do(ctx, "POST", "/auth/token", map[string]any{"user_id": userID, "role": role}, headers)
		if r.err == nil {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	if r.err != nil {
		return "", fmt.Errorf("token for %s: %w", userID, r.err)
	}
	if r.status != 200 || str(r.body, "token") == "" {
		return "", fmt.Errorf("token for %s failed %d: %s", userID, r.status, short(r.raw))
	}
	return str(r.body, "token"), nil
}

// tokens mints one user token per user, at most `limit` in flight.
func (a *api) tokens(ctx context.Context, users []string, limit int) (map[string]string, error) {
	out := make(map[string]string, len(users))
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, limit)
	for _, u := range users {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			t, err := a.token(ctx, u, "user")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			out[u] = t
		}(u)
	}
	wg.Wait()
	return out, firstErr
}

// ── shows ───────────────────────────────────────────────────────────────────

func (a *api) createShow(ctx context.Context, seats []string, perUserLimit, price int) (string, error) {
	r := a.do(ctx, "POST", "/shows", map[string]any{
		"name":           "Burst Test",
		"price_paise":    price,
		"per_user_limit": perUserLimit,
		"seats":          seats,
	}, map[string]string{"Authorization": "Bearer " + a.adminToken})
	if r.err != nil {
		return "", fmt.Errorf("create show: %w", r.err)
	}
	if r.status != 201 || str(r.body, "id") == "" {
		return "", fmt.Errorf("create show failed %d: %s", r.status, short(r.raw))
	}
	return str(r.body, "id"), nil
}

type showState struct{ available, held, confirmed, total int }

func (a *api) getShow(ctx context.Context, id string) (showState, error) {
	r := a.do(ctx, "GET", "/shows/"+id, nil, nil)
	if r.err != nil {
		return showState{}, r.err
	}
	if r.status != 200 || r.body == nil {
		return showState{}, fmt.Errorf("GET /shows/%s: %d %s", id, r.status, short(r.raw))
	}
	var s showState
	var ok [4]bool
	s.available, ok[0] = num(r.body, "available")
	s.held, ok[1] = num(r.body, "held")
	s.confirmed, ok[2] = num(r.body, "confirmed")
	s.total, ok[3] = num(r.body, "total_seats")
	if !(ok[0] && ok[1] && ok[2] && ok[3]) {
		return s, fmt.Errorf("GET /shows/%s: missing count fields", id)
	}
	return s, nil
}

func assertInvariant(s showState) bool {
	sum := s.available + s.held + s.confirmed
	ok := sum == s.total
	tag := pass
	if !ok {
		tag = fail
	}
	fmt.Printf("  invariant: available(%d)+held(%d)+confirmed(%d)=%d  total_seats=%d  %s\n",
		s.available, s.held, s.confirmed, sum, s.total, tag)
	return ok
}

// ── reserve ─────────────────────────────────────────────────────────────────

type result struct {
	seat, user, reason, resID string
	status                    int // 0 = client-side error (connection, timeout, …)
	dur                       time.Duration
}

type job struct {
	user, ikey, token string
	seats             []string
}

func (a *api) reserve(ctx context.Context, showID string, j job) result {
	sorted := append([]string(nil), j.seats...)
	sort.Strings(sorted)
	res := result{seat: strings.Join(sorted, ","), user: j.user}

	r := a.do(ctx, "POST", "/shows/"+showID+"/reserve",
		map[string]any{"seats": j.seats},
		map[string]string{"Authorization": "Bearer " + j.token, "Idempotency-Key": j.ikey})
	res.dur = r.dur
	if r.err != nil {
		res.reason = r.err.Error()
		return res
	}
	res.status = r.status
	if r.status == 201 {
		res.resID = str(r.body, "reservation_id")
	} else {
		res.reason = str(r.body, "error")
	}
	return res
}

// fire runs all jobs with at most `limit` in flight; results keep job order.
func (a *api) fire(ctx context.Context, showID string, jobs []job, limit int) []result {
	if limit <= 0 || limit > len(jobs) {
		limit = len(jobs)
	}
	results := make([]result, len(jobs))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = a.reserve(ctx, showID, jobs[i])
		}(i)
	}
	wg.Wait()
	return results
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.0fms", float64(d)/float64(time.Millisecond)) }

// ── phase 1: main burst ─────────────────────────────────────────────────────

func phase1(ctx context.Context, a *api, cfg config) (bool, error) {
	fmt.Println("\n══════════════ Phase 1: Main burst ══════════════")

	all := make([]string, cfg.seats)
	for i := range all {
		all[i] = fmt.Sprintf("S%04d", i+1)
	}
	hot, cold := all[:cfg.hot], all[cfg.hot:]

	showID, err := a.createShow(ctx, all, 4, 100)
	if err != nil {
		return false, err
	}
	fmt.Printf("Show %s | %d seats (%d hot, %d cold)\n", showID, cfg.seats, len(hot), len(cold))

	// Mint every user token before the timed section (not part of the burst).
	half := cfg.requests / 2
	nCold := cfg.requests - half
	users := make([]string, 0, cfg.requests)
	for i := 0; i < half; i++ {
		users = append(users, fmt.Sprintf("hot-u%d", i))
	}
	for i := 0; i < nCold; i++ {
		users = append(users, fmt.Sprintf("cold-u%d", i))
	}
	fmt.Printf("Minting %d user tokens via /auth/token …\n", len(users))
	tMint := time.Now()
	toks, err := a.tokens(ctx, users, 200)
	if err != nil {
		return false, err
	}
	fmt.Printf("  minted in %.1fs\n", time.Since(tMint).Seconds())

	jobs := make([]job, 0, cfg.requests+cfg.requests/20+1)
	for i := 0; i < half; i++ {
		u := fmt.Sprintf("hot-u%d", i)
		jobs = append(jobs, job{u, fmt.Sprintf("h%d-%d", i, time.Now().UnixNano()),
			toks[u], []string{hot[rand.Intn(len(hot))]}})
	}
	coldJobs := make([]job, 0, nCold)
	for i := 0; i < nCold; i++ {
		u := fmt.Sprintf("cold-u%d", i)
		coldJobs = append(coldJobs, job{u, fmt.Sprintf("c%d-%d", i, time.Now().UnixNano()),
			toks[u], []string{cold[i%len(cold)]}})
	}
	jobs = append(jobs, coldJobs...)

	// ~5% exact duplicate-key replays of cold requests.
	nDups := len(coldJobs) / 20
	if nDups < 1 && len(coldJobs) > 0 {
		nDups = 1
	}
	jobs = append(jobs, coldJobs[:nDups]...)
	rand.Shuffle(len(jobs), func(i, j int) { jobs[i], jobs[j] = jobs[j], jobs[i] })

	// Background invariant poller. Connection errors on these GETs are counted
	// separately: they say nothing about the invariant.
	var (
		violations []string
		pollErrs   int
		vmu        sync.Mutex
		pollWg     sync.WaitGroup
	)
	stop := make(chan struct{})
	pollWg.Add(1)
	go func() {
		defer pollWg.Done()
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			s, err := a.getShow(ctx, showID)
			vmu.Lock()
			if err != nil {
				if isTransportErr(err) {
					pollErrs++
				} else {
					violations = append(violations, "GET error: "+err.Error())
				}
			} else if s.available+s.held+s.confirmed != s.total {
				violations = append(violations, fmt.Sprintf("av=%d held=%d conf=%d sum=%d total=%d",
					s.available, s.held, s.confirmed, s.available+s.held+s.confirmed, s.total))
			}
			vmu.Unlock()
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()

	limit := cfg.clientCap
	capNote := "no cap — all at once"
	if limit > 0 {
		capNote = fmt.Sprintf("max %d in flight", limit)
	}
	fmt.Printf("Firing %d requests (%d duplicate-key replays mixed in, %s) …\n", len(jobs), nDups, capNote)

	before, scrapeErr := a.scrapeCounters(ctx)
	t0 := time.Now()
	results := a.fire(ctx, showID, jobs, limit)
	elapsed := time.Since(t0)
	close(stop)
	pollWg.Wait()
	time.Sleep(5 * time.Second) // let requests whose responses were dropped finish server-side
	after, scrapeErr2 := a.scrapeCounters(ctx)

	// ── tally ──
	statusCounts := map[int]int{}
	reasonCounts := map[string]int{}
	winners := map[string][]string{} // seat → reservation_ids of 201s
	var errSamples []string
	var durs []time.Duration
	for _, r := range results {
		statusCounts[r.status]++
		if r.status == 409 {
			reason := r.reason
			if reason == "" {
				reason = "unknown"
			}
			reasonCounts[reason]++
		}
		if r.status == 201 {
			winners[r.seat] = append(winners[r.seat], r.resID)
		}
		if r.status == 0 {
			if len(errSamples) < 5 {
				errSamples = append(errSamples, r.reason)
			}
		} else {
			durs = append(durs, r.dur)
		}
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	phase1ConnErrs = statusCounts[0]

	fmt.Printf("\nCompleted in %.2fs  (%.0f req/s)\n", elapsed.Seconds(), float64(len(jobs))/elapsed.Seconds())
	if len(durs) > 0 {
		fmt.Printf("  latency       : p50=%s  p95=%s  p99=%s  max=%s\n",
			ms(percentile(durs, 0.50)), ms(percentile(durs, 0.95)),
			ms(percentile(durs, 0.99)), ms(durs[len(durs)-1]))
	}
	fmt.Printf("  201 confirmed : %d\n", statusCounts[201])
	fmt.Printf("  409 declined  : %d\n", statusCounts[409])
	type kv struct {
		k string
		v int
	}
	var reasons []kv
	for k, v := range reasonCounts {
		reasons = append(reasons, kv{k, v})
	}
	sort.Slice(reasons, func(i, j int) bool { return reasons[i].v > reasons[j].v })
	for _, r := range reasons {
		fmt.Printf("    ↳ %s: %d\n", r.k, r.v)
	}
	var otherCodes []int
	for c := range statusCounts {
		if c != 201 && c != 409 && c != 0 {
			otherCodes = append(otherCodes, c)
		}
	}
	sort.Ints(otherCodes)
	for _, c := range otherCodes {
		fmt.Printf("  %-14d: %d\n", c, statusCounts[c])
	}
	if statusCounts[0] > 0 {
		fmt.Printf("  %-14s: %d\n", "ERR", statusCounts[0])
	}
	accounted := 0
	for _, n := range statusCounts {
		accounted += n
	}
	fmt.Printf("  accounted     : %d of %d fired\n", accounted, len(jobs))
	if len(errSamples) > 0 {
		fmt.Println("  ERR samples:")
		for _, e := range errSamples {
			fmt.Printf("    %s\n", e)
		}
	}

	// ── server-side reconciliation ──
	if scrapeErr == nil && scrapeErr2 == nil {
		printReconciliation(len(jobs), statusCounts[201], statusCounts[409], statusCounts[0], before, after)
	} else {
		fmt.Printf("\n  (reconciliation skipped: %v / %v)\n", scrapeErr, scrapeErr2)
	}

	// ── recovery: retry connection errors with the original Idempotency-Key ──
	unrecovered := 0
	if statusCounts[0] > 0 {
		var failed []job
		for i, r := range results {
			if r.status == 0 {
				failed = append(failed, jobs[i])
			}
		}
		rec := a.recoverFailed(ctx, showID, failed, 3, 200)
		recCounts := map[string]int{}
		for _, r := range rec {
			switch r.status {
			case 201:
				winners[r.seat] = append(winners[r.seat], r.resID)
				recCounts["201"]++
			case 409:
				recCounts["409 "+r.reason]++
			case 0:
				unrecovered++
				recCounts["still failing"]++
			default:
				unrecovered++
				recCounts[fmt.Sprintf("%d %s", r.status, r.reason)]++
			}
		}
		fmt.Printf("\n  Recovery: %d connection-error requests retried with original Idempotency-Key\n", len(failed))
		for k, v := range recCounts {
			fmt.Printf("    %-24s: %d\n", k, v)
		}
	}

	ok := true

	// A seat with several 201s is only a bug if the reservation_ids differ;
	// the same id means idempotent replays, which is correct.
	var doubled []string
	replay201s := 0
	unique := map[string]bool{}
	for seat, ids := range winners {
		distinct := map[string]bool{}
		for _, id := range ids {
			distinct[id] = true
			unique[id] = true
		}
		if len(distinct) > 1 {
			doubled = append(doubled, seat)
		} else {
			replay201s += len(ids) - 1
		}
	}
	if len(doubled) > 0 {
		sort.Strings(doubled)
		if len(doubled) > 3 {
			doubled = doubled[:3]
		}
		fmt.Printf("\n  %s  seat(s) double-booked (different reservation_ids): %v\n", fail, doubled)
		ok = false
	} else {
		note := ""
		if replay201s > 0 {
			note = fmt.Sprintf("  (%d replay 201s from idempotency, correct)", replay201s)
		}
		fmt.Printf("\n  %s  per-seat winner: no seat double-booked%s\n", pass, note)
	}

	show, err := a.getShow(ctx, showID)
	if err != nil {
		return false, err
	}
	if show.confirmed != len(unique) {
		fmt.Printf("  %s  GET confirmed=%d != unique reservations=%d\n", fail, show.confirmed, len(unique))
		ok = false
	} else {
		fmt.Printf("  %s  GET confirmed=%d matches unique reservations\n", pass, show.confirmed)
	}
	if !assertInvariant(show) {
		ok = false
	}

	n5xx := 0
	for c, n := range statusCounts {
		if c >= 500 {
			n5xx += n
		}
	}
	if n5xx > 0 || unrecovered > 0 {
		fmt.Printf("  %s  %d 5xx responses, %d requests unrecovered after retry\n", fail, n5xx, unrecovered)
		ok = false
	} else if statusCounts[0] > 0 {
		fmt.Printf("  WARN  zero 5xx; %d connection errors in burst, all recovered by idempotent retry\n", statusCounts[0])
	} else {
		fmt.Printf("  %s  zero 5xx / connection errors\n", pass)
	}

	// Every reserve must end as a domain outcome (201 or 409). Anything else,
	// e.g. 401 from a bad token, means the burst never tested anything.
	other := 0
	for c, n := range statusCounts {
		if c != 201 && c != 409 && c > 0 && c < 500 {
			other += n
		}
	}
	if other > 0 {
		fmt.Printf("  %s  %d responses were neither 201 nor 409 (401 → token problem)\n", fail, other)
		ok = false
	} else {
		fmt.Printf("  %s  every response was 201 or 409\n", pass)
	}

	if len(violations) > 0 {
		fmt.Printf("  %s  %d invariant violation(s) during burst:\n", fail, len(violations))
		for i, v := range violations {
			if i == 3 {
				break
			}
			fmt.Printf("    %s\n", v)
		}
		ok = false
	} else {
		fmt.Printf("  %s  no invariant violations during burst polling\n", pass)
	}
	if pollErrs > 0 {
		fmt.Printf("  INFO  %d invariant-poll GETs hit connection errors (not violations)\n", pollErrs)
	}

	sum := show.available + show.held + show.confirmed
	tag := pass
	if sum != show.total {
		tag = fail
	}
	fmt.Println("\n  Reconciliation:")
	fmt.Printf("    total_seats : %d\n", show.total)
	fmt.Printf("    confirmed   : %d\n", show.confirmed)
	fmt.Printf("    available   : %d\n", show.available)
	fmt.Printf("    held        : %d\n", show.held)
	fmt.Printf("    sum check   : %d+%d+%d=%d == %d  %s\n",
		show.available, show.held, show.confirmed, sum, show.total, tag)

	return ok, nil
}

// ── phase 2: per-user limit ─────────────────────────────────────────────────

func phase2(ctx context.Context, a *api, _ config) (bool, error) {
	fmt.Println("\n══════════════ Phase 2: Per-user limit ══════════════")
	const limit, nSeats = 4, 10
	labels := make([]string, nSeats)
	for i := range labels {
		labels[i] = fmt.Sprintf("L%03d", i+1)
	}
	showID, err := a.createShow(ctx, labels, limit, 100)
	if err != nil {
		return false, err
	}
	fmt.Printf("Show %s | %d seats, per_user_limit=%d\n", showID, nSeats, limit)
	fmt.Printf("Firing %d parallel single-seat reserves for the same user …\n", nSeats)

	uid := "limit-test-user"
	tok, err := a.token(ctx, uid, "user")
	if err != nil {
		return false, err
	}
	jobs := make([]job, nSeats)
	for i := range jobs {
		jobs[i] = job{uid, fmt.Sprintf("lim-%d-%d", i, time.Now().UnixNano()), tok, []string{labels[i]}}
	}
	results := a.fire(ctx, showID, jobs, nSeats)

	okCount, limitHits := 0, 0
	var unexpected []string
	for _, r := range results {
		if r.status == 201 {
			okCount++
		}
		if r.reason == "per_user_limit" {
			limitHits++
		}
		if r.status != 201 && r.status != 409 {
			unexpected = append(unexpected, fmt.Sprintf("(%d %s)", r.status, r.reason))
		}
	}
	fmt.Printf("  201 confirmed           : %d  (expected %d)\n", okCount, limit)
	fmt.Printf("  409 per_user_limit      : %d  (expected %d)\n", limitHits, nSeats-limit)

	ok := true
	if okCount != limit {
		fmt.Printf("  %s  expected %d confirmed, got %d\n", fail, limit, okCount)
		ok = false
	} else {
		fmt.Printf("  %s  exactly %d seats confirmed\n", pass, limit)
	}
	if limitHits != nSeats-limit {
		fmt.Printf("  %s  expected %d per_user_limit declines, got %d\n", fail, nSeats-limit, limitHits)
		ok = false
	} else {
		fmt.Printf("  %s  remaining %d correctly declined as per_user_limit\n", pass, nSeats-limit)
	}
	if len(unexpected) > 0 {
		fmt.Printf("  %s  unexpected responses: %s\n", fail, strings.Join(unexpected, " "))
		ok = false
	}
	return ok, nil
}

// ── phase 3: idempotency ────────────────────────────────────────────────────

func phase3(ctx context.Context, a *api, _ config) (bool, error) {
	fmt.Println("\n══════════════ Phase 3: Idempotency ══════════════")
	showID, err := a.createShow(ctx, []string{"I001", "I002"}, 4, 100)
	if err != nil {
		return false, err
	}
	uid := "idem-user"
	tok, err := a.token(ctx, uid, "user")
	if err != nil {
		return false, err
	}
	ikey := fmt.Sprintf("idem-key-%d", time.Now().UnixNano())
	ok := true

	r1 := a.reserve(ctx, showID, job{uid, ikey, tok, []string{"I001"}})
	if r1.status != 201 {
		fmt.Printf("  %s  first reserve: expected 201, got %d %s\n", fail, r1.status, r1.reason)
		return false, nil
	}
	fmt.Printf("  First reserve → 201, reservation_id=%s\n", r1.resID)

	r2 := a.reserve(ctx, showID, job{uid, ikey, tok, []string{"I001"}})
	switch {
	case r2.status != 201:
		fmt.Printf("  %s  replay: expected 201, got %d %s\n", fail, r2.status, r2.reason)
		ok = false
	case r2.resID != r1.resID:
		fmt.Printf("  %s  replay: reservation_id changed %s → %s\n", fail, r1.resID, r2.resID)
		ok = false
	default:
		fmt.Printf("  %s  replay returns same reservation_id (%s)\n", pass, r1.resID)
	}

	s, err := a.getShow(ctx, showID)
	if err != nil {
		return false, err
	}
	if s.confirmed != 1 {
		fmt.Printf("  %s  replay caused extra confirmation: confirmed=%d\n", fail, s.confirmed)
		ok = false
	} else {
		fmt.Printf("  %s  confirmed=1 after replay (no duplicate seat claimed)\n", pass)
	}

	r3 := a.reserve(ctx, showID, job{uid, ikey, tok, []string{"I002"}})
	if r3.status == 409 && r3.reason == "idempotent_conflict" {
		fmt.Printf("  %s  same key + different seat → 409 idempotent_conflict\n", pass)
	} else {
		fmt.Printf("  %s  same key + different seat: expected 409 idempotent_conflict, got %d %q\n",
			fail, r3.status, r3.reason)
		ok = false
	}
	return ok, nil
}

// ── phase 4: identity ───────────────────────────────────────────────────────

func phase4(ctx context.Context, a *api, _ config) (bool, error) {
	fmt.Println("\n══════════════ Phase 4: Identity ══════════════")
	showID, err := a.createShow(ctx, []string{"ID001", "ID002"}, 4, 100)
	if err != nil {
		return false, err
	}
	ok := true

	realUID := "real-user"
	realTok, err := a.token(ctx, realUID, "user")
	if err != nil {
		return false, err
	}
	otherTok, err := a.token(ctx, "other-user", "user")
	if err != nil {
		return false, err
	}

	r := a.do(ctx, "POST", "/shows/"+showID+"/reserve",
		map[string]any{"seats": []string{"ID001"}, "user_id": "someone-else"},
		map[string]string{
			"Authorization":   "Bearer " + realTok,
			"Idempotency-Key": fmt.Sprintf("id-spoof-%d", time.Now().UnixNano()),
		})
	if r.err != nil || r.status != 201 {
		fmt.Printf("  %s  spoofed body: expected 201, got %d %s %v\n", fail, r.status, short(r.raw), r.err)
		ok = false
	} else if got := str(r.body, "user_id"); got == realUID {
		fmt.Printf("  %s  spoofed body user_id ignored; response user_id = token's (%s)\n", pass, realUID)
	} else {
		fmt.Printf("  %s  response user_id=%q, expected %q\n", fail, got, realUID)
		ok = false
	}

	resID := ""
	if r.err == nil && r.status == 201 {
		resID = str(r.body, "reservation_id")
	}
	if resID == "" {
		fmt.Println("  SKIP  no reservation_id to test cancel ownership (earlier step failed)")
		return ok, nil
	}
	cr := a.do(ctx, "POST", "/reservations/"+resID+"/cancel", nil,
		map[string]string{"Authorization": "Bearer " + otherTok})
	if cr.err == nil && cr.status == 404 {
		fmt.Printf("  %s  cancel by wrong user → 404 (no reservation ID leak)\n", pass)
	} else {
		fmt.Printf("  %s  cancel by wrong user: expected 404, got %d %v\n", fail, cr.status, cr.err)
		ok = false
	}
	return ok, nil
}

// ── entry point ─────────────────────────────────────────────────────────────

func parseArgs(args []string) (config, error) {
	fs := flag.NewFlagSet("burst", flag.ContinueOnError)
	var c config
	fs.IntVar(&c.requests, "concurrency", 300, "total reserve requests in phase 1")
	fs.IntVar(&c.seats, "seats", 60, "total seats in the phase-1 show")
	fs.IntVar(&c.hot, "hot", 5, "number of contested hot seats")
	fs.IntVar(&c.phase, "phase", 0, "run only this phase (1-4); 0 runs all")
	fs.IntVar(&c.clientCap, "client-cap", 0, "max requests in flight at once (0 = all at once)")
	fs.DurationVar(&c.timeout, "timeout", 30*time.Second, "per-request timeout")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: burst [BASE_URL] [flags]   (ADMIN_KEY env required)")
		fs.PrintDefaults()
	}

	// Allow the URL before the flags, as burst.sh and the README pass it.
	base := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		base, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if base == "" && fs.NArg() > 0 {
		base = fs.Arg(0)
	}
	if base == "" {
		base = os.Getenv("BASE_URL")
	}
	if base == "" {
		base = "http://localhost:8080"
	}
	c.baseURL = strings.TrimRight(base, "/")

	c.adminKey = os.Getenv("ADMIN_KEY")

	switch {
	case c.adminKey == "":
		return c, errors.New("ADMIN_KEY is required (local docker-compose: local-admin-key)")
	case c.phase < 0 || c.phase > 4:
		return c, errors.New("--phase must be 1-4 (or 0 for all)")
	case c.hot < 1 || c.hot >= c.seats:
		return c, errors.New("--hot must be at least 1 and less than --seats")
	case c.requests < 2:
		return c, errors.New("--concurrency must be at least 2")
	}
	return c, nil
}

func main() {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	if os.Getenv("NO_COLOR") != "" {
		pass, fail = "PASS", "FAIL"
	}

	inFlight := cfg.clientCap
	if inFlight <= 0 {
		inFlight = cfg.requests + cfg.requests/20 + 1
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	dial := dialer.DialContext
	if os.Getenv("SPREAD_IPS") == "1" {
		d, ips, err := spreadDialer(dialer, cfg.baseURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(2)
		}
		dial = d
		fmt.Printf("Spreading connections across %d IPs: %v\n", len(ips), ips)
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         dial,
		MaxIdleConns:        inFlight + 10,
		MaxIdleConnsPerHost: inFlight + 10, // default is 2, which would churn connections
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableCompression:  true,
	}
	a := &api{
		base:     cfg.baseURL,
		client:   &http.Client{Transport: transport, Timeout: cfg.timeout},
		adminKey: cfg.adminKey,
	}
	fmt.Printf("Target: %s\n", cfg.baseURL)

	adminTok, err := a.token(context.Background(), "burst-admin", "admin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: admin token:", err, "(check ADMIN_KEY)")
		os.Exit(2)
	}
	a.adminToken = adminTok

	type phase struct {
		n     int
		label string
		run   func(context.Context, *api, config) (bool, error)
	}
	phases := []phase{
		{1, "Main burst", phase1},
		{2, "Per-user limit", phase2},
		{3, "Idempotency", phase3},
		{4, "Identity", phase4},
	}

	ctx := context.Background()
	type outcome struct {
		p  phase
		ok bool
	}
	var outcomes []outcome
	for _, p := range phases {
		if cfg.phase != 0 && cfg.phase != p.n {
			continue
		}
		ok, err := p.run(ctx, a, cfg)
		if err != nil {
			fmt.Printf("  %s  phase %d aborted: %v\n", fail, p.n, err)
			ok = false
		}
		outcomes = append(outcomes, outcome{p, ok})
	}

	fmt.Println("\n══════════════ Summary ══════════════")
	allOK := true
	for _, o := range outcomes {
		tag := pass
		if !o.ok {
			tag = fail
			allOK = false
		}
		fmt.Printf("  Phase %d  %-20s  %s\n", o.p.n, o.p.label, tag)
	}
	if phase1ConnErrs > 0 {
		fmt.Printf("  WARN     Phase 1 connection errors: %d (see Recovery above)\n", phase1ConnErrs)
	}
	if !allOK {
		os.Exit(1)
	}
}
