package main

//
//   scrapeCounters      reads the app's reservation counters from /actuator/prometheus
//   printReconciliation splits connection errors into "lost after the app answered"
//                       vs "never reached the app"
//   recoverFailed       re-sends connection-error jobs with the SAME token, body and
//                       Idempotency-Key, as a real client would
//   isTransportErr      tells a connection error apart from an HTTP-level error
//
// The counter math assumes a single ECS task (counters are per-JVM) and no other
// reserve traffic between the before/after scrapes.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type counters struct{ confirmed, seatTaken, replay float64 }

func (a *api) scrapeCounters(ctx context.Context) (counters, error) {
	var c counters
	r := a.do(ctx, "GET", "/actuator/prometheus", nil, nil)
	if r.err != nil {
		return c, r.err
	}
	if r.status != 200 {
		return c, fmt.Errorf("/actuator/prometheus: HTTP %d", r.status)
	}
	sc := bufio.NewScanner(bytes.NewReader(r.raw))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(f[len(f)-1], 64)
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "reservations_confirmed_total"):
			c.confirmed += v
		case strings.HasPrefix(line, "reservations_declined_total") && strings.Contains(line, `reason="seat_taken"`):
			c.seatTaken += v
		case strings.HasPrefix(line, "reservations_declined_total") && strings.Contains(line, `reason="idempotent_replay"`):
			c.replay += v
		}
	}
	return c, sc.Err()
}

func printReconciliation(fired, got201, got409, connErrs int, before, after counters) {
	dConf := int(after.confirmed - before.confirmed)
	dDecl := int(after.seatTaken - before.seatTaken)
	dRepl := int(after.replay - before.replay)
	processed := dConf + dDecl + dRepl
	received := got201 + got409
	lost := processed - received
	lost409 := dDecl - got409
	never := fired - processed

	fmt.Println("\n  Server-side reconciliation (app counters, before/after burst):")
	fmt.Printf("    fired              : %d\n", fired)
	fmt.Printf("    processed by app   : %d  (confirmed %d, seat_taken %d, replay %d)\n", processed, dConf, dDecl, dRepl)
	fmt.Printf("    received by client : %d\n", received)
	fmt.Printf("    lost after app     : %d  (409s %d, 201s %d)\n", lost, lost409, lost-lost409)
	fmt.Printf("    never reached app  : %d\n", never)
	_ = connErrs // lost+never always equals fired-received, so it is not a check
	switch {
	case never < 0:
		fmt.Printf("    WARN  app processed %d more requests than were fired: some were re-sent\n", -never)
		fmt.Println("          by the HTTP client, so the lost/never split above is not valid")
	case lost < 0:
		fmt.Println("    WARN  client received more than the app counted (more than one task running?)")
	}
}

// recoverFailed re-fires failed jobs (same token, body, Idempotency-Key) for up to
// `rounds` rounds with backoff, at most `limit` in flight. Results align with `failed`.
func (a *api) recoverFailed(ctx context.Context, showID string, failed []job, rounds, limit int) []result {
	out := make([]result, len(failed))
	pending := make([]int, len(failed))
	for i := range pending {
		pending[i] = i
	}
	backoff := 500 * time.Millisecond
	for round := 0; round < rounds && len(pending) > 0; round++ {
		if round > 0 {
			time.Sleep(backoff)
			backoff *= 2
		}
		batch := make([]job, len(pending))
		for k, i := range pending {
			batch[k] = failed[i]
		}
		res := a.fire(ctx, showID, batch, limit)
		var still []int
		for k, i := range pending {
			out[i] = res[k]
			if res[k].status == 0 {
				still = append(still, i)
			}
		}
		pending = still
	}
	return out
}

// isTransportErr is true for connection-level failures (EOF, reset, timeout), which
// http.Client returns as *url.Error; getShow's own HTTP/parse errors are plain errors.
func isTransportErr(err error) bool {
	var ue *url.Error
	return errors.As(err, &ue)
}
