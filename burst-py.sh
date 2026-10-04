#!/usr/bin/env bash
# One-command burst runner.
#
# Usage:
#   ./burst.sh [BASE_URL] [burst.py options]
#
# Ramp up before going to 20k:
#   ./burst.sh                                    # default 300 concurrent
#   CONCURRENCY=2000  ./burst.sh                  # ramp 1
#   CONCURRENCY=5000  ./burst.sh                  # ramp 2
#   CONCURRENCY=20000 ./burst.sh                  # full grader load
#   CONCURRENCY=5000  ./burst.sh --client-cap 1000  # throttle client side

set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"

ulimit -n 65536 2>/dev/null || true
ACTUAL_ULIMIT=$(ulimit -n)
echo "fd limit: $ACTUAL_ULIMIT"
if [ "$ACTUAL_ULIMIT" -lt 4096 ]; then
    echo "WARNING: fd limit is low ($ACTUAL_ULIMIT). Connection errors may be OS-level, not server failures."
fi

SECRET="${JWT_SECRET:-change-me-in-production-this-is-at-least-32-bytes-long!!}"
if [ "$SECRET" = "change-me-in-production-this-is-at-least-32-bytes-long!!" ]; then
    echo "INFO: JWT_SECRET not set — using application.properties default."
fi

# First positional arg is BASE_URL if it looks like a URL, otherwise use env/default.
if [[ "${1:-}" == http* ]]; then
    _BASE="$1"
    shift
else
    _BASE="${BASE_URL:-http://localhost:8080}"
fi

python3 "$DIR/tools/loadgen-py/burst.py" \
  "$_BASE" \
  --concurrency "${CONCURRENCY:-300}" \
  --seats       "${SEATS:-60}" \
  --hot         "${HOT:-5}" \
  "$@"
EXIT_CODE=$?

echo ""
echo "══════════════ CloudWatch (last 15 min) ══════════════"
"$DIR/aws-metrics.sh" 15

exit $EXIT_CODE
