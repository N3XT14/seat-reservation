#!/usr/bin/env bash
export PYTHONUNBUFFERED=1
aws --profile seat-sandbox --region us-east-2 logs tail /ecs/seat-reservation \
  --follow --since "${1:-1m}" --format short \
  | jq -Rr --unbuffered 'try (capture("(?<j>\\{.*)$").j | fromjson
      | "\(."@timestamp"[11:23])  \(.log.level)  \(.request_id // "-")  \(.message)") catch empty'
