#!/usr/bin/env bash
# usage: ./scripts/release-app.sh [tag]      (default tag: current git commit)
# Builds the app image, pushes it to the ECR repo already used in
# infra/seat-svc-taskdef.json, writes the new image into that file, and registers
# a new task definition revision FROM THE FILE, so the repo is what gets deployed.
# Then roll it out with: ./scripts/deploy-app.sh <revision>
# Needs: docker (buildx), jq, git, aws CLI.
set -euo pipefail
cd "$(dirname "$0")/.."

TD_FILE=infra/seat-svc-taskdef.json
AWS="aws --profile seat-sandbox --region us-east-2"

# The image must match a commit, so refuse to build uncommitted app code.
if [ -n "$(git status --porcelain -- src Dockerfile pom.xml build.gradle build.gradle.kts)" ]; then
  echo "error: uncommitted changes in app code; commit first" >&2
  exit 1
fi
TAG="${1:-$(git rev-parse --short HEAD)}"

OLD_IMAGE=$(jq -r '.containerDefinitions[] | select(.name=="app") | .image' "$TD_FILE")
REPO="${OLD_IMAGE%:*}"
REGISTRY="${REPO%%/*}"
ARCH=$(jq -r '.runtimePlatform.cpuArchitecture // "X86_64"' "$TD_FILE")
if [ "$ARCH" = "ARM64" ]; then PLATFORM=linux/arm64; else PLATFORM=linux/amd64; fi
NEW_IMAGE="$REPO:$TAG"
echo "current : $OLD_IMAGE"
echo "new     : $NEW_IMAGE  [$PLATFORM]"

$AWS ecr get-login-password | docker login --username AWS --password-stdin "$REGISTRY"
docker buildx build --platform "$PLATFORM" -t "$NEW_IMAGE" --push .

jq --arg img "$NEW_IMAGE" \
  '.containerDefinitions |= map(if .name == "app" then .image = $img else . end)' \
  "$TD_FILE" > "$TD_FILE.tmp" && mv "$TD_FILE.tmp" "$TD_FILE"

REV=$($AWS ecs register-task-definition --cli-input-json "file://$TD_FILE" \
  --query 'taskDefinition.revision' --output text)
echo "registered: $(jq -r .family "$TD_FILE"):$REV"
echo "next      : ./scripts/deploy-app.sh $REV"
