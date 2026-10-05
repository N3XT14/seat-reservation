#!/usr/bin/env bash
# Run the Go loadgen as a Fargate task against the NLB, wait for it to finish, print results.
#
# Usage:
#   ./run-burst.sh                      # uses $URL, 20000 requests, uncapped
#   CAP=2000 ./run-burst.sh             # client-side cap on in-flight requests
#   REQUESTS=5000 SEATS=100 HOT=10 ./run-burst.sh
#   QUIET=1 ./run-burst.sh              # warm-up run: no log output
set -euo pipefail

export AWS_PROFILE="${AWS_PROFILE:-seat-sandbox}"
export AWS_REGION="${AWS_REGION:-us-east-2}"

# URL="${URL:-http://seat-alb-v2-811419728.us-east-2.elb.amazonaws.com}"
URL="${URL:-http://seat-nlb-a1a61f5739a9984f.elb.us-east-2.amazonaws.com}"
REQUESTS="${REQUESTS:-20000}"
SEATS="${SEATS:-200}"
HOT="${HOT:-20}"
CAP="${CAP:-0}"            # 0 = all at once
QUIET="${QUIET:-0}"
TASK_CPU="${TASK_CPU:-2048}"
TASK_MEM="${TASK_MEM:-4096}"
SPREAD_IPS="${SPREAD_IPS:-0}"

CLUSTER=seat
TASK_DEF=seat-loadgen
LOG_GROUP=/ecs/seat-reservation
NETWORK='awsvpcConfiguration={subnets=[subnet-060e461ede7c6bc58,subnet-0f638bec6a0078f39],securityGroups=[sg-0934cd31e4076ddbf],assignPublicIp=ENABLED}'
ADMIN_KEY="${ADMIN_KEY:-$(aws ssm get-parameter --name /seat/admin-key --with-decryption \
  --query Parameter.Value --output text)}"
OVERRIDES=$(printf '{"cpu":"%s","memory":"%s","containerOverrides":[{"name":"loadgen","environment":[{"name":"SPREAD_IPS","value":"%s"},{"name":"ADMIN_KEY","value":"%s"}],"command":["%s","--concurrency","%s","--seats","%s","--hot","%s","--client-cap","%s"]}]}' \
  "$TASK_CPU" "$TASK_MEM" "$SPREAD_IPS" "$ADMIN_KEY" "$URL" "$REQUESTS" "$SEATS" "$HOT" "$CAP")

echo "Burst: requests=$REQUESTS seats=$SEATS hot=$HOT cap=$CAP -> $URL"

TASK=$(aws ecs run-task --cluster "$CLUSTER" --launch-type FARGATE --task-definition "$TASK_DEF" \
  --network-configuration "$NETWORK" --overrides "$OVERRIDES" \
  --query 'tasks[0].taskArn' --output text)
TASK_ID=${TASK##*/}
echo "task: $TASK_ID  (started $(date '+%H:%M:%S'))"

aws ecs wait tasks-running --cluster "$CLUSTER" --tasks "$TASK" && echo "RUNNING"
aws ecs wait tasks-stopped --cluster "$CLUSTER" --tasks "$TASK" && echo "DONE    (finished $(date '+%H:%M:%S'))"

EXIT_CODE=$(aws ecs describe-tasks --cluster "$CLUSTER" --tasks "$TASK" \
  --query 'tasks[0].containers[0].exitCode' --output text)
echo "exit code: $EXIT_CODE"

if [[ "$QUIET" != "1" ]]; then
  aws logs get-log-events --log-group-name "$LOG_GROUP" \
    --log-stream-name "loadgen/loadgen/$TASK_ID" --start-from-head \
    --query 'events[].message' --output text | tr '\t' '\n'
fi
