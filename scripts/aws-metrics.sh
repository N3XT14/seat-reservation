#!/usr/bin/env bash
# usage: ./aws-metrics.sh 15     -> last 15 minutes
# Prints ECS, RDS and load balancer metrics: NLB (the submitted URL) always,
# ALB only if it still exists.
MIN="${1:-15}"
AWSP="--profile seat-sandbox --region us-east-2"
P="$AWSP --period 60 --start-time $(date -u -v-${MIN}M +%FT%TZ) --end-time $(date -u +%FT%TZ)"
m() { local title="$1" stat="$2"; shift 2
  echo "== $title"
  aws cloudwatch get-metric-statistics $P --statistics "$stat" \
    --query "sort_by(Datapoints,&Timestamp)[].[Timestamp,$stat]" --output text "$@"; }

# lb_dims <name>  ->  "<LoadBalancer dimension> <TargetGroup dimension>", or fails if no such LB
lb_dims() {
  local arn tg
  arn=$(aws elbv2 describe-load-balancers $AWSP --names "$1" \
    --query 'LoadBalancers[0].LoadBalancerArn' --output text 2>/dev/null) || return 1
  [ -n "$arn" ] && [ "$arn" != "None" ] || return 1
  tg=$(aws elbv2 describe-target-groups $AWSP --load-balancer-arn "$arn" \
    --query 'TargetGroups[0].TargetGroupArn' --output text)
  echo "${arn#*:loadbalancer/} ${tg##*:}"
}

m "ECS CPU max %"    Maximum --namespace AWS/ECS --metric-name CPUUtilization    --dimensions Name=ClusterName,Value=seat Name=ServiceName,Value=seat-svc
m "ECS memory max %" Maximum --namespace AWS/ECS --metric-name MemoryUtilization --dimensions Name=ClusterName,Value=seat Name=ServiceName,Value=seat-svc
m "RDS CPU max %"    Maximum --namespace AWS/RDS --metric-name CPUUtilization    --dimensions Name=DBInstanceIdentifier,Value=seat-db
m "RDS connections"  Maximum --namespace AWS/RDS --metric-name DatabaseConnections --dimensions Name=DBInstanceIdentifier,Value=seat-db

NLB_NAME="${NLB_NAME:-seat-nlb}"
if read -r LB TG < <(lb_dims "$NLB_NAME"); then
  NLB="--namespace AWS/NetworkELB"
  LBD="--dimensions Name=LoadBalancer,Value=$LB"
  TGD="--dimensions Name=LoadBalancer,Value=$LB Name=TargetGroup,Value=$TG"
  m "NLB ActiveFlowCount max"     Maximum $NLB --metric-name ActiveFlowCount $LBD
  m "NLB NewFlowCount"            Sum     $NLB --metric-name NewFlowCount $LBD
  m "NLB ProcessedBytes"          Sum     $NLB --metric-name ProcessedBytes $LBD
  m "NLB TCP client resets"       Sum     $NLB --metric-name TCP_Client_Reset_Count $LBD
  m "NLB TCP target resets"       Sum     $NLB --metric-name TCP_Target_Reset_Count $LBD
  m "NLB TCP ELB resets"          Sum     $NLB --metric-name TCP_ELB_Reset_Count $LBD
  m "NLB HealthyHostCount min"    Minimum $NLB --metric-name HealthyHostCount $TGD
  m "NLB UnHealthyHostCount max"  Maximum $NLB --metric-name UnHealthyHostCount $TGD
else
  echo "== NLB $NLB_NAME not found, skipping"
fi

ALB_NAME="${ALB_NAME:-seat-alb-v2}"
if read -r LB TG < <(lb_dims "$ALB_NAME"); then
  ALB="--namespace AWS/ApplicationELB"
  LBD="--dimensions Name=LoadBalancer,Value=$LB"
  TGD="--dimensions Name=LoadBalancer,Value=$LB Name=TargetGroup,Value=$TG"
  m "ALB TargetResponseTime max (s)" Maximum $ALB --metric-name TargetResponseTime $LBD
  m "ALB RequestCount"               Sum     $ALB --metric-name RequestCount $LBD
  m "ALB NewConnectionCount"         Sum     $ALB --metric-name NewConnectionCount $LBD
  m "ALB ActiveConnectionCount"      Sum     $ALB --metric-name ActiveConnectionCount $LBD
  m "ALB RejectedConnectionCount"    Sum     $ALB --metric-name RejectedConnectionCount $LBD
  m "ALB TargetConnectionErrorCount" Sum     $ALB --metric-name TargetConnectionErrorCount $LBD
  m "ALB ELB 5xx"                    Sum     $ALB --metric-name HTTPCode_ELB_5XX_Count $LBD
  m "ALB Target 5xx"                 Sum     $ALB --metric-name HTTPCode_Target_5XX_Count $LBD
  m "ALB HealthyHostCount min"       Minimum $ALB --metric-name HealthyHostCount $TGD
fi