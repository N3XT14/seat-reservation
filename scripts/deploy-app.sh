#!/usr/bin/env bash
# usage: ./deploy-app.sh 9   -> roll seat-svc to seat-reservation:9 and wait until stable
set -euo pipefail
REV="${1:?usage: $0 <revision>   e.g. $0 9}"
AWS="aws --profile seat-sandbox --region us-east-2"
$AWS ecs update-service --cluster seat --service seat-svc \
  --task-definition "seat-reservation:$REV" --query 'service.taskDefinition' --output text
echo "waiting for rollout to finish…"
$AWS ecs wait services-stable --cluster seat --services seat-svc
echo "stable on seat-reservation:$REV"
