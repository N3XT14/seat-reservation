#!/usr/bin/env bash
# usage: ./release-loadgen.sh v5
# Vets/builds tools/loadgen, pushes it to the same ECR repo the seat-loadgen task def
# already uses, and registers a new seat-loadgen revision pointing at the new tag.
# Needs: docker (buildx), jq, go, aws CLI.  Override FAMILY / CTX via env if needed.
set -euo pipefail
TAG="${1:?usage: $0 <tag>   e.g. $0 v5}"
FAMILY="${FAMILY:-seat-loadgen}"
CTX="${CTX:-tools/loadgen}"          # directory containing the loadgen Dockerfile
AWS="aws --profile seat-sandbox --region us-east-2"

TD=$($AWS ecs describe-task-definition --task-definition "$FAMILY" --query taskDefinition --output json)
OLD_IMAGE=$(jq -r '.containerDefinitions[0].image' <<<"$TD")
REPO="${OLD_IMAGE%:*}"
REGISTRY="${REPO%%/*}"
ARCH=$(jq -r '.runtimePlatform.cpuArchitecture // "X86_64"' <<<"$TD")
if [ "$ARCH" = "ARM64" ]; then PLATFORM=linux/arm64; else PLATFORM=linux/amd64; fi
NEW_IMAGE="$REPO:$TAG"
echo "current : $OLD_IMAGE (rev $(jq -r .revision <<<"$TD"))"
echo "new     : $NEW_IMAGE  [$PLATFORM]"

( cd "$CTX" && go vet ./... && go build -o /dev/null . )

$AWS ecr get-login-password | docker login --username AWS --password-stdin "$REGISTRY"
docker buildx build --platform "$PLATFORM" -t "$NEW_IMAGE" --push "$CTX"

NEW_TD=$(jq --arg img "$NEW_IMAGE" '
  .containerDefinitions[0].image = $img
  | del(.taskDefinitionArn, .revision, .status, .requiresAttributes, .compatibilities,
        .registeredAt, .registeredBy, .deregisteredAt)' <<<"$TD")
echo -n "registered: "
$AWS ecs register-task-definition --cli-input-json "$NEW_TD" \
  --query 'taskDefinition.[family,revision]' --output text
