#!/usr/bin/env bash
set -Eeuo pipefail

# SituationAwareness Agent Docker deployment script.
# Run on a Linux node that already has Docker installed:
#   sudo bash install-agent-docker.sh
#
# Optional non-secret overrides:
#   sudo env AGENT_NAME=node-bj-01 HOST_PORT=8002 BIND_ADDRESS=0.0.0.0 \
#     bash install-agent-docker.sh
#
# For unattended deployment, provide secrets through protected files:
#   sudo env \
#     DOCKERHUB_TOKEN_FILE=/run/secrets/dockerhub_pull_token \
#     AGENT_SHARED_TOKEN_FILE=/run/secrets/agent_shared_token \
#     bash install-agent-docker.sh

IMAGE_REF="${IMAGE_REF:-beiou/situationawareness-agent:1.0.0}"
DOCKERHUB_USERNAME="${DOCKERHUB_USERNAME:-beiou}"
CONTAINER_NAME="${CONTAINER_NAME:-situation-awareness-agent}"
CONFIG_DIR="${CONFIG_DIR:-/etc/situation-awareness-agent}"
ENV_FILE="${ENV_FILE:-${CONFIG_DIR}/agent.env}"
BIND_ADDRESS="${BIND_ADDRESS:-0.0.0.0}"
HOST_PORT="${HOST_PORT:-8002}"
AGENT_NAME="${AGENT_NAME:-$(hostname -f 2>/dev/null || hostname)}"
AGENT_MAX_CONCURRENT="${AGENT_MAX_CONCURRENT:-8}"
AGENT_DEFAULT_TIMEOUT="${AGENT_DEFAULT_TIMEOUT:-10s}"
AGENT_MAX_TIMEOUT="${AGENT_MAX_TIMEOUT:-30s}"
CONTAINER_LABEL="com.situation-awareness.service=agent"

log() {
  printf '[deploy] %s\n' "$*"
}

fail() {
  printf '[deploy] ERROR: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

read_secret_file() {
  local path="$1"
  [[ -r "$path" ]] || fail "secret file is not readable: $path"
  local value
  value="$(tr -d '\r\n' < "$path")"
  [[ -n "$value" ]] || fail "secret file is empty: $path"
  printf '%s' "$value"
}

validate_no_whitespace() {
  local name="$1"
  local value="$2"
  [[ -n "$value" ]] || fail "$name cannot be empty"
  [[ "$value" != *[[:space:]]* ]] || fail "$name must not contain whitespace"
}

health_request() {
  local url="$1"
  if command -v curl >/dev/null 2>&1; then
    curl --fail --silent --show-error --max-time 3 "$url"
  elif command -v wget >/dev/null 2>&1; then
    wget --quiet --timeout=3 --output-document=- "$url"
  else
    return 127
  fi
}

if [[ "${EUID}" -ne 0 ]]; then
  fail "run this script as root, for example: sudo bash $0"
fi

require_command docker
require_command hostname
require_command install
require_command tr
require_command grep

if ! command -v curl >/dev/null 2>&1 &&
  ! command -v wget >/dev/null 2>&1; then
  fail "curl or wget is required for the health check"
fi

docker info >/dev/null 2>&1 || fail "Docker daemon is not running or is not accessible"

[[ "$HOST_PORT" =~ ^[0-9]+$ ]] || fail "HOST_PORT must be numeric"
(( HOST_PORT >= 1 && HOST_PORT <= 65535 )) || fail "HOST_PORT must be between 1 and 65535"
[[ "$BIND_ADDRESS" =~ ^[0-9.]+$ ]] || fail "BIND_ADDRESS must be an IPv4 address"
validate_no_whitespace "AGENT_NAME" "$AGENT_NAME"

DOCKERHUB_TOKEN="${DOCKERHUB_TOKEN:-}"
if [[ -n "${DOCKERHUB_TOKEN_FILE:-}" ]]; then
  DOCKERHUB_TOKEN="$(read_secret_file "$DOCKERHUB_TOKEN_FILE")"
elif [[ -z "$DOCKERHUB_TOKEN" ]]; then
  read -r -s -p "Docker Hub Read-only token for ${DOCKERHUB_USERNAME}: " DOCKERHUB_TOKEN
  printf '\n'
fi
validate_no_whitespace "DOCKERHUB_TOKEN" "$DOCKERHUB_TOKEN"

log "logging in to Docker Hub as ${DOCKERHUB_USERNAME}"
printf '%s' "$DOCKERHUB_TOKEN" |
  docker login --username "$DOCKERHUB_USERNAME" --password-stdin
unset DOCKERHUB_TOKEN

AGENT_SHARED_TOKEN="${AGENT_SHARED_TOKEN:-}"
if [[ -n "${AGENT_SHARED_TOKEN_FILE:-}" ]]; then
  AGENT_SHARED_TOKEN="$(read_secret_file "$AGENT_SHARED_TOKEN_FILE")"
elif [[ -z "$AGENT_SHARED_TOKEN" ]]; then
  require_command od
  AGENT_SHARED_TOKEN="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \r\n')"
  log "generated a unique Agent API token"
fi
validate_no_whitespace "AGENT_SHARED_TOKEN" "$AGENT_SHARED_TOKEN"
(( ${#AGENT_SHARED_TOKEN} >= 32 )) ||
  fail "AGENT_SHARED_TOKEN must contain at least 32 characters"

log "pulling ${IMAGE_REF}"
docker pull "$IMAGE_REF"

install -d -m 0700 "$CONFIG_DIR"
install -m 0600 /dev/null "$ENV_FILE"
{
  printf 'AGENT_LISTEN_ADDR=:8002\n'
  printf 'AGENT_NAME=%s\n' "$AGENT_NAME"
  printf 'AGENT_SHARED_TOKEN=%s\n' "$AGENT_SHARED_TOKEN"
  printf 'AGENT_MAX_CONCURRENT=%s\n' "$AGENT_MAX_CONCURRENT"
  printf 'AGENT_DEFAULT_TIMEOUT=%s\n' "$AGENT_DEFAULT_TIMEOUT"
  printf 'AGENT_MAX_TIMEOUT=%s\n' "$AGENT_MAX_TIMEOUT"
} > "$ENV_FILE"
unset AGENT_SHARED_TOKEN

if docker container inspect "$CONTAINER_NAME" >/dev/null 2>&1; then
  existing_label="$(
    docker container inspect \
      --format '{{ index .Config.Labels "com.situation-awareness.service" }}' \
      "$CONTAINER_NAME"
  )"
  [[ "$existing_label" == "agent" ]] ||
    fail "container ${CONTAINER_NAME} exists but is not managed by this script"
  log "replacing existing container ${CONTAINER_NAME}"
  docker container rm --force "$CONTAINER_NAME" >/dev/null
fi

log "starting ${CONTAINER_NAME} on ${BIND_ADDRESS}:${HOST_PORT}"
docker run --detach \
  --name "$CONTAINER_NAME" \
  --restart unless-stopped \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --pids-limit 128 \
  --memory 256m \
  --cpus 1 \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  --env-file "$ENV_FILE" \
  --publish "${BIND_ADDRESS}:${HOST_PORT}:8002" \
  --label "$CONTAINER_LABEL" \
  "$IMAGE_REF" >/dev/null

health_host="$BIND_ADDRESS"
if [[ "$BIND_ADDRESS" == "0.0.0.0" ]]; then
  health_host="127.0.0.1"
fi
health_url="http://${health_host}:${HOST_PORT}/healthz"

log "waiting for ${health_url}"
health_response=""
for ((attempt = 1; attempt <= 30; attempt++)); do
  if health_response="$(health_request "$health_url" 2>/dev/null)"; then
    break
  fi
  if ! docker container inspect \
    --format '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null |
    grep -qx true; then
    break
  fi
  sleep 1
done

if [[ -z "$health_response" ]]; then
  docker container logs --tail 100 "$CONTAINER_NAME" >&2 || true
  fail "Agent did not pass its health check"
fi

log "health check passed: ${health_response}"
log "container: ${CONTAINER_NAME}"
log "image: ${IMAGE_REF}"
log "configuration: ${ENV_FILE}"
log "view logs: docker logs --tail 100 ${CONTAINER_NAME}"
log "read the Agent API token locally with:"
printf "  sed -n 's/^AGENT_SHARED_TOKEN=//p' %q\n" "$ENV_FILE"

if [[ "$BIND_ADDRESS" == "0.0.0.0" ]]; then
  printf '\n'
  printf '[deploy] SECURITY WARNING: TCP %s is published on every host interface.\n' "$HOST_PORT"
  printf '[deploy] Allow this port only from the Master public IP in the cloud firewall/security group.\n'
  printf '[deploy] The current Agent endpoint is HTTP; use an HTTPS reverse proxy for production traffic.\n'
fi
