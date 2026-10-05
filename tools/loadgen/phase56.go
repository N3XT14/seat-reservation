package main

// Phase 5 — Multi-seat overlap: many users reserve overlapping adjacent pairs at
//
//	once, half listing the seats in reverse order. Proves the server's
//	deterministic lock order (no deadlock → no 5xx/429) and all-or-nothing
//	(confirmed == 2 × pair reservations, no half-claimed pair).
//
// Phase 6 — Cancel vs rebook: every seat is held by an owner; each owner cancels
//
//	twice in parallel while 50 racers per seat try to grab it. Proves exactly
//	one cancel wins, a released seat goes to at most one new owner, the owner
//	never keeps it, the invariant holds throughout, and leftovers rebook cleanly.

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"
)

// ── shared helpers ──────────────────────────────────────────────────────────

// invPoller GETs the show every 200ms and records any invariant violation.
type invPoller struct {
	stop       chan struct{}
	wg         sync.WaitGroup
	mu         sync.Mutex
	violations []string
	errs       int
}

func startPoller(ctx context.Context, a *api, showID string) *invPoller {
	p := &invPoller{stop: make(chan struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			s, err := a.getShow(ctx, showID)
			p.mu.Lock()
			switch {
			case err != nil && isTransportErr(err):
				p.errs++
			case err != nil:
				p.violations = append(p.violations, "GET error: "+err.Error())
			case s.available+s.held+s.confirmed != s.total:
				p.violations = append(p.violations, fmt.Sprintf("av=%d held=%d conf=%d total=%d",
					s.available, s.held, s.confirmed, s.total))
			}
			p.mu.Unlock()
			select {
			case <-p.stop:
				return
			case <-t.C:
			}
		}
	}()
	return p
}

// finish stops the poller and prints its verdict.
func (p *invPoller) finish() bool {
	close(p.stop)
	p.wg.Wait()
	if p.errs > 0 {
		fmt.Printf("  INFO  %d invariant-poll GETs hit connection errors (not violations)\n", p.errs)
	}
	if len(p.violations) > 0 {
		fmt.Printf("  %s  %d invariant violation(s) during the race, e.g. %s\n", fail, len(p.violations), p.violations[0])
		return false
	}
	fmt.Printf("  %s  no invariant violations during polling\n", pass)
	return true
}

// recoverConnErrors retries connection-error reserves with their original
// Idempotency-Key, so a request the server did process can't skew the counts.
func recoverConnErrors(ctx context.Context, a *api, showID string, jobs []job, results []result) bool {
	still := 0
	retried := 0
	for i := range results {
		if results[i].status != 0 {
			continue
		}
		retried++
		var r result
		for attempt := 0; attempt < 3; attempt++ {
			r = a.reserve(ctx, showID, jobs[i])
			if r.status != 0 {
				break
			}
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
		}
		results[i] = r
		if r.status == 0 {
			still++
		}
	}
	if retried > 0 {
		fmt.Printf("  Recovery: %d connection-error requests retried with original Idempotency-Key, %d still failing\n", retried, still)
	}
	return still == 0
}

// checkReserveOutcomes fails on anything that isn't a 201 or a 409 with an
// allowed reason. A 429 here would mean lock contention (e.g. a deadlock)
// surfaced as "busy", which is exactly what these phases exist to rule out.
func checkReserveOutcomes(results []result, allowed ...string) bool {
	okReason := map[string]bool{}
	for _, r := range allowed {
		okReason[r] = true
	}
	status := map[int]int{}
	reasons := map[string]int{}
	var bad []string
	badN := 0
	for _, r := range results {
		status[r.status]++
		if r.status == 409 {
			reasons[r.reason]++
		}
		if r.status == 201 || (r.status == 409 && okReason[r.reason]) {
			continue
		}
		badN++
		if len(bad) < 3 {
			bad = append(bad, fmt.Sprintf("%d %s", r.status, r.reason))
		}
	}
	fmt.Printf("  201 confirmed : %d\n", status[201])
	fmt.Printf("  409 declined  : %d\n", status[409])
	for k, v := range reasons {
		fmt.Printf("    ↳ %s: %d\n", k, v)
	}
	if badN > 0 {
		fmt.Printf("  %s  %d responses were not 201 or an expected 409 (429 = lock contention surfaced as busy), e.g. %s\n",
			fail, badN, strings.Join(bad, ", "))
		return false
	}
	fmt.Printf("  %s  zero 5xx / 429: every response was 201 or 409 %v\n", pass, allowed)
	return true
}

// ── phase 5: overlapping multi-seat ─────────────────────────────────────────

func phase5(ctx context.Context, a *api, cfg config) (bool, error) {
	fmt.Println("\n══════════════ Phase 5: Overlapping multi-seat ══════════════")
	const nSeats = 40
	nUsers := cfg.requests / 10
	if nUsers < 200 {
		nUsers = 200
	}
	if nUsers > 2000 {
		nUsers = 2000
	}

	labels := make([]string, nSeats)
	for i := range labels {
		labels[i] = fmt.Sprintf("M%03d", i+1)
	}
	showID, err := a.createShow(ctx, labels, 4, 100)
	if err != nil {
		return false, err
	}
	fmt.Printf("Show %s | %d seats | %d users, each wants an adjacent pair (half listed in reverse order)\n",
		showID, nSeats, nUsers)

	users := make([]string, nUsers)
	for i := range users {
		users[i] = fmt.Sprintf("pair-u%d", i)
	}
	toks, err := a.tokens(ctx, users, 200)
	if err != nil {
		return false, err
	}

	stamp := time.Now().UnixNano()
	jobs := make([]job, nUsers)
	for i, u := range users {
		k := rand.Intn(nSeats - 1)
		pair := []string{labels[k], labels[k+1]}
		if i%2 == 1 {
			pair[0], pair[1] = pair[1], pair[0] // server must sort before locking
		}
		jobs[i] = job{u, fmt.Sprintf("p%d-%d", i, stamp), toks[u], pair}
	}

	poll := startPoller(ctx, a, showID)
	t0 := time.Now()
	results := a.fire(ctx, showID, jobs, cfg.clientCap)
	fmt.Printf("Fired %d pair requests in %.2fs\n", len(jobs), time.Since(t0).Seconds())

	ok := recoverConnErrors(ctx, a, showID, jobs, results)
	if !checkReserveOutcomes(results, "seat_taken") {
		ok = false
	}

	// No seat may belong to two different reservations.
	owner := map[string]string{}
	unique := map[string]bool{}
	var clash []string
	for _, r := range results {
		if r.status != 201 {
			continue
		}
		unique[r.resID] = true
		for _, s := range strings.Split(r.seat, ",") {
			if prev, seen := owner[s]; seen && prev != r.resID {
				clash = append(clash, s)
			}
			owner[s] = r.resID
		}
	}
	if len(clash) > 0 {
		sort.Strings(clash)
		fmt.Printf("  %s  seat(s) in two reservations: %v\n", fail, clash)
		ok = false
	} else {
		fmt.Printf("  %s  no seat appears in two pair reservations\n", pass)
	}

	show, err := a.getShow(ctx, showID)
	if err != nil {
		return false, err
	}
	if show.confirmed != 2*len(unique) {
		fmt.Printf("  %s  GET confirmed=%d != 2 × %d pair reservations (half-claimed pair?)\n",
			fail, show.confirmed, len(unique))
		ok = false
	} else {
		fmt.Printf("  %s  GET confirmed=%d == 2 × %d pair reservations (all-or-nothing held)\n",
			pass, show.confirmed, len(unique))
	}
	if !assertInvariant(show) {
		ok = false
	}
	if !poll.finish() {
		ok = false
	}
	return ok, nil
}

// ── phase 6: cancel vs rebook race ──────────────────────────────────────────

func phase6(ctx context.Context, a *api, _ config) (bool, error) {
	fmt.Println("\n══════════════ Phase 6: Cancel vs rebook race ══════════════")
	const nSeats, racersPerSeat = 20, 50

	labels := make([]string, nSeats)
	for i := range labels {
		labels[i] = fmt.Sprintf("R%03d", i+1)
	}
	showID, err := a.createShow(ctx, labels, 4, 100)
	if err != nil {
		return false, err
	}

	var users []string
	for i := 0; i < nSeats; i++ {
		users = append(users, fmt.Sprintf("own-u%d", i), fmt.Sprintf("rebook-u%d", i))
		for k := 0; k < racersPerSeat; k++ {
			users = append(users, fmt.Sprintf("race-u%d-%d", i, k))
		}
	}
	toks, err := a.tokens(ctx, users, 200)
	if err != nil {
		return false, err
	}
	stamp := time.Now().UnixNano()

	// Setup: each owner holds one seat.
	ownerJobs := make([]job, nSeats)
	for i := range labels {
		u := fmt.Sprintf("own-u%d", i)
		ownerJobs[i] = job{u, fmt.Sprintf("o%d-%d", i, stamp), toks[u], []string{labels[i]}}
	}
	setup := a.fire(ctx, showID, ownerJobs, 0)
	resIDs := make([]string, nSeats)
	for i, r := range setup {
		if r.status != 201 {
			return false, fmt.Errorf("setup reserve %s: %d %s", labels[i], r.status, r.reason)
		}
		resIDs[i] = r.resID
	}

	var racerJobs []job
	for i := range labels {
		for k := 0; k < racersPerSeat; k++ {
			u := fmt.Sprintf("race-u%d-%d", i, k)
			racerJobs = append(racerJobs, job{u, fmt.Sprintf("r%d-%d-%d", i, k, stamp), toks[u], []string{labels[i]}})
		}
	}
	fmt.Printf("Show %s | %d seats, each held by its owner; %d double-cancels race %d rebook attempts …\n",
		showID, nSeats, nSeats, len(racerJobs))

	poll := startPoller(ctx, a, showID)
	start := make(chan struct{})
	var wg sync.WaitGroup

	cancelDelay := make([]time.Duration, nSeats)
	for i := range cancelDelay {
		cancelDelay[i] = time.Duration(rand.Intn(600)) * time.Millisecond
	}

	cancels := make([]response, 2*nSeats)
	for i := 0; i < nSeats; i++ {
		for k := 0; k < 2; k++ {
			wg.Add(1)
			go func(i, slot int) {
				defer wg.Done()
				<-start
				time.Sleep(cancelDelay[i])
				cancels[slot] = a.do(ctx, "POST", "/reservations/"+resIDs[i]+"/cancel", nil,
					map[string]string{"Authorization": "Bearer " + ownerJobs[i].token})
			}(i, 2*i+k)
		}
	}
	racing := make([]result, len(racerJobs))
	for i := range racerJobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// Spread racers over ~300ms so some land before, during and after the cancels commit.
			time.Sleep(time.Duration(rand.Intn(300)) * time.Millisecond)
			racing[i] = a.reserve(ctx, showID, racerJobs[i])
		}(i)
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	fmt.Printf("Race finished in %.2fs\n", time.Since(t0).Seconds())

	ok := true

	// Cancels: exactly one 200 and one 409 already_cancelled per reservation.
	badCancels, cancelErrs := 0, 0
	for i := 0; i < nSeats; i++ {
		c1, c2 := cancels[2*i], cancels[2*i+1]
		if c1.err != nil || c2.err != nil {
			cancelErrs++
			continue
		}
		codes := []int{c1.status, c2.status}
		sort.Ints(codes)
		loser := c1
		if c2.status == 409 {
			loser = c2
		}
		if codes[0] != 200 || codes[1] != 409 || str(loser.body, "error") != "already_cancelled" {
			badCancels++
		}
	}
	switch {
	case cancelErrs > 0:
		fmt.Printf("  %s  %d cancel pairs hit connection errors (can't verify exactly-once)\n", fail, cancelErrs)
		ok = false
	case badCancels > 0:
		fmt.Printf("  %s  %d of %d reservations did not get exactly one 200 + one 409 already_cancelled\n",
			fail, badCancels, nSeats)
		ok = false
	default:
		fmt.Printf("  %s  every double-cancel: exactly one 200, one 409 already_cancelled\n", pass)
	}

	// Racers.
	if !recoverConnErrors(ctx, a, showID, racerJobs, racing) {
		ok = false
	}
	if !checkReserveOutcomes(racing, "seat_taken") {
		ok = false
	}
	winners := map[string]map[string]bool{}
	for _, r := range racing {
		if r.status != 201 {
			continue
		}
		if winners[r.seat] == nil {
			winners[r.seat] = map[string]bool{}
		}
		winners[r.seat][r.resID] = true
	}
	var doubled []string
	for seat, ids := range winners {
		if len(ids) > 1 {
			doubled = append(doubled, seat)
		}
	}
	if len(doubled) > 0 {
		sort.Strings(doubled)
		fmt.Printf("  %s  released seat(s) won by two racers: %v\n", fail, doubled)
		ok = false
	} else {
		fmt.Printf("  %s  each released seat went to at most one racer (%d of %d re-won during the race)\n",
			pass, len(winners), nSeats)
	}

	// Every owner cancelled, so every confirmed seat must be a racer's.
	show, err := a.getShow(ctx, showID)
	if err != nil {
		return false, err
	}
	if show.confirmed != len(winners) {
		fmt.Printf("  %s  GET confirmed=%d but racers won %d seats (owner kept a cancelled seat, or a seat was double-sold)\n",
			fail, show.confirmed, len(winners))
		ok = false
	} else {
		fmt.Printf("  %s  GET confirmed=%d == racer wins (no cancelled seat stayed with its owner)\n", pass, show.confirmed)
	}
	if !assertInvariant(show) {
		ok = false
	}

	// Re-bookability: any seat still free must be cleanly bookable now.
	var rebookJobs []job
	for i, l := range labels {
		if winners[l] == nil {
			u := fmt.Sprintf("rebook-u%d", i)
			rebookJobs = append(rebookJobs, job{u, fmt.Sprintf("b%d-%d", i, stamp), toks[u], []string{l}})
		}
	}
	if len(rebookJobs) > 0 {
		reb := a.fire(ctx, showID, rebookJobs, 0)
		good := 0
		for _, r := range reb {
			if r.status == 201 {
				good++
			}
		}
		if good != len(rebookJobs) {
			fmt.Printf("  %s  rebooking leftover seats: %d of %d succeeded\n", fail, good, len(rebookJobs))
			ok = false
		} else {
			fmt.Printf("  %s  %d leftover released seats rebooked cleanly\n", pass, good)
		}
		show, err = a.getShow(ctx, showID)
		if err != nil {
			return false, err
		}
		if !assertInvariant(show) {
			ok = false
		}
	}

	if !poll.finish() {
		ok = false
	}
	return ok, nil
}
