#!/usr/bin/env bash
# One-command burst runner.
#
# Usage:
#   ./burst.sh [BASE_URL] [flags]
#
#   ./burst.sh http://<host>                                           # smoke run (300 requests)
#   CONCURRENCY=20000 SEATS=200 HOT=20 ./burst.sh http://<host> --client-cap 2000
#   ./burst.sh http://<host> --phase 1                                 # one phase only
#

set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
LOADGEN="$DIR/tools/loadgen"

ulimit -n 65536 2>/dev/null || true

if [[ "${1:-}" == http* ]]; then
    BASE="$1"
    shift
else
    BASE="${BASE_URL:-http://localhost:8080}"
fi

ARGS=("$BASE" --concurrency "${CONCURRENCY:-300}" --seats "${SEATS:-60}" --hot "${HOT:-5}" "$@")

if command -v go >/dev/null 2>&1; then
    (cd "$LOADGEN" && go run . "${ARGS[@]}")
    EXIT_CODE=$?
else
    docker build -q -t seat-loadgen "$LOADGEN" >/dev/null || exit 1
    NET=()
    [[ "$BASE" == *localhost* || "$BASE" == *127.0.0.1* ]] && NET=(--network host)
    docker run --rm "${NET[@]}" -e JWT_SECRET="${JWT_SECRET:-}" seat-loadgen "${ARGS[@]}"
    EXIT_CODE=$?
fi

# CloudWatch summary: only for someone with access to the AWS account.
if [ -x "$DIR/aws-metrics.sh" ] && command -v aws >/dev/null 2>&1 \
   && aws sts get-caller-identity --profile seat-sandbox >/dev/null 2>&1; then
    echo ""
    echo "══════════════ CloudWatch (last 15 min) ══════════════"
    "$DIR/aws-metrics.sh" 15
fi

exit $EXIT_CODE
