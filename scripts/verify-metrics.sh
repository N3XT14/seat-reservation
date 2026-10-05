#!/usr/bin/env bash
# Walks reserve / replay / conflict / taken / limit / partial / cancel / rebook on a
# fresh show and prints API counts, gauges and counters after every step, then checks
# gauge == API and available + held + confirmed == total.
#
# Usage: ADMIN_KEY=... scripts/verify-metrics.sh [BASE_URL]
#   GAUGE_WAIT  seconds to wait for the gauge refresher (1s)
set -euo pipefail

BASE="${1:-http://localhost:8080}"
: "${ADMIN_KEY:?set ADMIN_KEY}"
WAIT="${GAUGE_WAIT:-1.5}"
RUN="$(date +%s)"
FAILS=0
BODY=/tmp/vm_body


user_token()  { curl -sf -X POST "$BASE/auth/token" -H 'Content-Type: application/json' \
                  -d "{\"user_id\":\"$1\"}" | jq -r .token; }
admin_token() { curl -sf -X POST "$BASE/auth/token" -H 'Content-Type: application/json' \
                  -H "X-Admin-Key: $ADMIN_KEY" -d '{"user_id":"admin","role":"admin"}' | jq -r .token; }

reserve() { # token key seats_json
  curl -s -o "$BODY" -w '%{http_code}' -X POST "$BASE/shows/$SHOW/reserve" \
    -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -d "{\"seats\":$3,\"idempotency_key\":\"$2\"}"
}
cancel() { # token reservation_id
  curl -s -o "$BODY" -w '%{http_code}' -X POST "$BASE/reservations/$2/cancel" \
    -H "Authorization: Bearer $1"
}
step() { # label expected actual
  local mark=OK
  if [[ "$2" != "$3" ]]; then mark=FAIL; FAILS=$((FAILS+1)); fi
  printf '\n[%s] %-46s expected %s got %s\n     body: %s\n' "$mark" "$1" "$2" "$3" "$(head -c 200 "$BODY")"
}

g() { echo "$SCRAPE" | awk -v k="$1" '$1==k {print $2; f=1} END {if (!f) print "MISSING"}'; }
snap() {
  sleep "$WAIT"
  SCRAPE="$(curl -s "$BASE/actuator/prometheus")"
  API="$(curl -s "$BASE/shows/$SHOW" | jq -r '"\(.available) \(.held) \(.confirmed) \(.total_seats)"')"
  local q='"'
  local s="show_id=${q}${SHOW}${q}"
  local ka="seats{${s},status=${q}available${q}}"
  local kh="seats{${s},status=${q}held${q}}"
  local kc="seats{${s},status=${q}confirmed${q}}"
  local kt="seats_capacity{${s}}"
  local kv="seats_available{${s}}"
  local va vh vc vt vv
  va=$(g "$ka"); vh=$(g "$kh"); vc=$(g "$kc"); vt=$(g "$kt"); vv=$(g "$kv")
  printf '     api   avail/held/conf/total = %s\n' "$API"
  printf '     gauge avail/held/conf/total = %s %s %s %s  seats_available=%s\n' \
    "$va" "$vh" "$vc" "$vt" "$vv"
  local aa ah ac at
  read -r aa ah ac at <<<"$API"
  if [[ "${va%.0} ${vh%.0} ${vc%.0} ${vt%.0} ${vv%.0}" != "$aa $ah $ac $at $aa" ]]; then
    echo "     [FAIL] gauge does not match API (refresh interval longer than GAUGE_WAIT?)"
    FAILS=$((FAILS+1))
  fi
  printf '     counters confirmed=%s replay=%s conflict=%s taken=%s limit=%s cancelled=%s busy=%s\n' \
    "$(g reservations_confirmed_total)" \
    "$(g 'reservations_declined_total{reason="idempotent_replay"}')" \
    "$(g 'reservations_declined_total{reason="idempotent_conflict"}')" \
    "$(g 'reservations_declined_total{reason="seat_taken"}')" \
    "$(g 'reservations_declined_total{reason="per_user_limit"}')" \
    "$(g reservations_cancelled_total)" "$(g db_busy_responses_total)"
}

ADMIN="$(admin_token)"; U1="$(user_token "u1-$RUN")"; U2="$(user_token "u2-$RUN")"

SHOW="$(curl -sf -X POST "$BASE/shows" -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d "{\"name\":\"verify-$RUN\",\"seats\":[\"A1\",\"A2\",\"A3\",\"A4\",\"A5\",\"A6\"],\"price_paise\":25000,\"per_user_limit\":4}" \
  | jq -r '.id // .show_id')"
echo "show $SHOW"
WAIT=0 snap; echo "     ^ immediately after create: gauges should already be 6/0/0/6"
snap

c=$(reserve "$U1" "k1-$RUN" '["A1"]');            step "U1 reserves A1"                     201 "$c"; snap
RID="$(jq -r .reservation_id "$BODY")"
c=$(reserve "$U1" "k1-$RUN" '["A1"]');            step "U1 replays same key"                201 "$c"
[[ "$(jq -r .reservation_id "$BODY")" == "$RID" ]] || { echo "     FAIL replay returned a different reservation"; FAILS=$((FAILS+1)); }
snap
c=$(reserve "$U1" "k1-$RUN" '["A2"]');            step "U1 same key, different seats"       409 "$c"; snap
c=$(reserve "$U2" "k2-$RUN" '["A1"]');            step "U2 takes taken A1"                  409 "$c"; snap
c=$(reserve "$U2" "k3-$RUN" '["A2","A3","A4","A5","A6"]'); step "U2 asks 5 (limit 4)"       409 "$c"; snap
c=$(reserve "$U2" "k4-$RUN" '["A1","A2"]');       step "U2 partial A1+A2 (all-or-nothing)"  409 "$c"; snap
c=$(cancel "$U2" "$RID");                         step "U2 cancels U1's reservation"        404 "$c"; snap
c=$(cancel "$U1" "$RID");                         step "U1 cancels own reservation"         200 "$c"; snap
c=$(cancel "$U1" "$RID");                         step "U1 cancels again"                   409 "$c"; snap
c=$(reserve "$U2" "k5-$RUN" '["A1"]');            step "U2 rebooks released A1"             201 "$c"; snap

read -r a h cf t <<<"$API"
ga="$(g "seats_available{show_id=\"$SHOW\"}")"
echo
if (( a + h + cf == t )); then echo "[OK]   invariant $a + $h + $cf == $t"; else echo "[FAIL] invariant $a + $h + $cf != $t"; FAILS=$((FAILS+1)); fi
if [[ "${ga%.0}" == "$a" ]]; then echo "[OK]   gauge seats_available=$ga matches API"; else echo "[FAIL] gauge $ga vs API $a"; FAILS=$((FAILS+1)); fi
echo; echo "failures: $FAILS"
exit $(( FAILS > 0 ))