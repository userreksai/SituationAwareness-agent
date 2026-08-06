#!/usr/bin/env bash
set -Eeuo pipefail

# Outbound-only SituationAwareness Agent Docker deployment.
# The Agent opens no host port and connects to Master itself.
#
# First deployment:
#   sudo env MASTER_HOST=10.0.0.10 AGENT_NAME=node-bj-01 \
#     bash install-agent-docker.sh
#
# Optional secret files for unattended deployment:
#   DOCKERHUB_TOKEN_FILE=/run/secrets/dockerhub_pull_token
#   AGENT_SHARED_TOKEN_FILE=/run/secrets/agent_shared_token
# Offline/private archive deployment:
#   DOWNLOAD_URL=http://MASTER_IP:8891/situation-awareness-agent-1.1.0.tar
#   DOWNLOAD_SHA256=<out-of-band trusted SHA-256; preferred>
#   DOWNLOAD_SHA256_URL=${DOWNLOAD_URL}.sha256

IMAGE_REF="${IMAGE_REF:-beiou/situationawareness-agent:1.1.0}"
DOCKERHUB_USERNAME="${DOCKERHUB_USERNAME:-beiou}"
CONTAINER_NAME="${CONTAINER_NAME:-situation-awareness-agent}"
ROLLBACK_CONTAINER="${ROLLBACK_CONTAINER:-${CONTAINER_NAME}-rollback}"
CONFIG_DIR="${CONFIG_DIR:-/etc/situation-awareness-agent}"
ENV_FILE="${ENV_FILE:-${CONFIG_DIR}/agent.env}"
MASTER_PORT="${MASTER_PORT:-9910}"
MASTER_CONNECT_PATH="${MASTER_CONNECT_PATH:-/api/v1/agent/connect}"
AGENT_MAX_CONCURRENT="${AGENT_MAX_CONCURRENT:-8}"
AGENT_DEFAULT_TIMEOUT="${AGENT_DEFAULT_TIMEOUT:-10s}"
AGENT_MAX_TIMEOUT="${AGENT_MAX_TIMEOUT:-30s}"
AGENT_RECONNECT_MIN="${AGENT_RECONNECT_MIN:-1s}"
AGENT_RECONNECT_MAX="${AGENT_RECONNECT_MAX:-30s}"
AGENT_HEARTBEAT_INTERVAL="${AGENT_HEARTBEAT_INTERVAL:-20s}"
WAIT_FOR_REGISTRATION="${WAIT_FOR_REGISTRATION:-false}"
DOWNLOAD_URL="${DOWNLOAD_URL:-}"
DOWNLOAD_SHA256="${DOWNLOAD_SHA256:-}"
DOWNLOAD_SHA256_URL="${DOWNLOAD_SHA256_URL:-}"
ALLOW_LEGACY_CONTAINER_MIGRATION="${ALLOW_LEGACY_CONTAINER_MIGRATION:-false}"
CONTAINER_LABEL="com.situation-awareness.service=agent"
docker_extra_args=()
previous_available=false
downloaded_image=""

cleanup_download() {
  if [[ -n "$downloaded_image" && -f "$downloaded_image" ]]; then
    rm -f -- "$downloaded_image"
  fi
}

trap cleanup_download EXIT

restore_previous_on_error() {
  local exit_code="$?"
  trap - ERR
  set +e
  if [[ "$previous_available" == "true" ]]; then
    printf '[deploy] new deployment failed; restoring previous container\n' >&2
    docker container rm --force "$CONTAINER_NAME" >/dev/null 2>&1 || true
    docker container rename "$ROLLBACK_CONTAINER" "$CONTAINER_NAME" >/dev/null 2>&1
    docker container start "$CONTAINER_NAME" >/dev/null 2>&1
  fi
  cleanup_download
  exit "$exit_code"
}

trap restore_previous_on_error ERR

log() {
  printf '[deploy] %s\n' "$*"
}

fail() {
  printf '[deploy] ERROR: %s\n' "$*" >&2
  return 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

read_secret_file() {
  local path="$1"
  [[ -r "$path" ]] || fail "secret file is not readable: $path"
  local value
  value="$(tr -d '\r\n' <"$path")"
  [[ -n "$value" ]] || fail "secret file is empty: $path"
  printf '%s' "$value"
}

read_existing_value() {
  local key="$1"
  [[ -r "$ENV_FILE" ]] || return 0
  sed -n "s/^${key}=//p" "$ENV_FILE" | tail -n 1
}

validate_no_whitespace() {
  local name="$1"
  local value="$2"
  [[ -n "$value" ]] || fail "$name cannot be empty"
  [[ "$value" != *[[:space:]]* ]] || fail "$name must not contain whitespace"
}

if [[ "${EUID}" -ne 0 ]]; then
  fail "run this script as root, for example: sudo bash $0"
fi

require_command docker
require_command hostname
require_command install
require_command sed
require_command tail
require_command tr
require_command grep
docker info >/dev/null 2>&1 || fail "Docker daemon is not running or is not accessible"

[[ "$MASTER_PORT" =~ ^[0-9]+$ ]] || fail "MASTER_PORT must be numeric"
((MASTER_PORT >= 1 && MASTER_PORT <= 65535)) || fail "MASTER_PORT must be between 1 and 65535"
[[ "$MASTER_CONNECT_PATH" == /* ]] || fail "MASTER_CONNECT_PATH must start with /"
[[ "$WAIT_FOR_REGISTRATION" == "true" || "$WAIT_FOR_REGISTRATION" == "false" ]] ||
  fail "WAIT_FOR_REGISTRATION must be true or false"
[[ "$ALLOW_LEGACY_CONTAINER_MIGRATION" == "true" || "$ALLOW_LEGACY_CONTAINER_MIGRATION" == "false" ]] ||
  fail "ALLOW_LEGACY_CONTAINER_MIGRATION must be true or false"
if [[ -n "${HOST_PORT:-}" || -n "${BIND_ADDRESS:-}" ]]; then
  fail "HOST_PORT and BIND_ADDRESS are obsolete; outbound-only Agent deployments publish no port"
fi
if [[ -n "$DOWNLOAD_URL" ]]; then
  validate_no_whitespace "DOWNLOAD_URL" "$DOWNLOAD_URL"
  if [[ -n "$DOWNLOAD_SHA256" ]]; then
    validate_no_whitespace "DOWNLOAD_SHA256" "$DOWNLOAD_SHA256"
    [[ "$DOWNLOAD_SHA256" =~ ^[0-9a-fA-F]{64}$ ]] ||
      fail "DOWNLOAD_SHA256 must be a SHA-256 value"
  else
    DOWNLOAD_SHA256_URL="${DOWNLOAD_SHA256_URL:-${DOWNLOAD_URL}.sha256}"
    validate_no_whitespace "DOWNLOAD_SHA256_URL" "$DOWNLOAD_SHA256_URL"
  fi
fi

existing_master_url="$(read_existing_value AGENT_MASTER_URL)"
existing_agent_name="$(read_existing_value AGENT_NAME)"
existing_agent_token="$(read_existing_value AGENT_SHARED_TOKEN)"

AGENT_NAME="${AGENT_NAME:-${existing_agent_name:-$(hostname -f 2>/dev/null || hostname)}}"
validate_no_whitespace "AGENT_NAME" "$AGENT_NAME"

AGENT_MASTER_URL="${AGENT_MASTER_URL:-$existing_master_url}"
if [[ -z "$AGENT_MASTER_URL" ]]; then
  MASTER_HOST="${MASTER_HOST:-}"
  validate_no_whitespace "MASTER_HOST" "$MASTER_HOST"
  AGENT_MASTER_URL="ws://${MASTER_HOST}:${MASTER_PORT}${MASTER_CONNECT_PATH}"
fi
validate_no_whitespace "AGENT_MASTER_URL" "$AGENT_MASTER_URL"
[[ "$AGENT_MASTER_URL" == ws://* || "$AGENT_MASTER_URL" == wss://* ]] ||
  fail "AGENT_MASTER_URL must start with ws:// or wss://"

AGENT_SHARED_TOKEN="${AGENT_SHARED_TOKEN:-}"
if [[ -n "${AGENT_SHARED_TOKEN_FILE:-}" ]]; then
  AGENT_SHARED_TOKEN="$(read_secret_file "$AGENT_SHARED_TOKEN_FILE")"
elif [[ -z "$AGENT_SHARED_TOKEN" && -n "$existing_agent_token" ]]; then
  AGENT_SHARED_TOKEN="$existing_agent_token"
  log "reusing the existing per-node Agent token"
elif [[ -z "$AGENT_SHARED_TOKEN" ]]; then
  require_command od
  AGENT_SHARED_TOKEN="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \r\n')"
  log "generated a new per-node Agent token"
fi
validate_no_whitespace "AGENT_SHARED_TOKEN" "$AGENT_SHARED_TOKEN"
((${#AGENT_SHARED_TOKEN} >= 32)) ||
  fail "AGENT_SHARED_TOKEN must contain at least 32 characters"

if [[ -n "$DOWNLOAD_URL" ]]; then
  require_command awk
  require_command curl
  require_command mktemp
  require_command rm
  require_command sha256sum
  downloaded_image="$(mktemp "${TMPDIR:-/tmp}/situation-awareness-agent.XXXXXX.tar")"
  log "downloading image archive from ${DOWNLOAD_URL}"
  curl --fail --silent --show-error --location "$DOWNLOAD_URL" --output "$downloaded_image"
  if [[ -n "$DOWNLOAD_SHA256" ]]; then
    expected_checksum="$DOWNLOAD_SHA256"
  else
    expected_checksum="$(
      curl --fail --silent --show-error --location "$DOWNLOAD_SHA256_URL" |
        awk 'NR == 1 { print $1; exit }'
    )"
  fi
  [[ "$expected_checksum" =~ ^[0-9a-fA-F]{64}$ ]] ||
    fail "downloaded checksum is not a SHA-256 value"
  actual_checksum="$(sha256sum "$downloaded_image" | awk '{ print $1 }')"
  [[ "${actual_checksum,,}" == "${expected_checksum,,}" ]] ||
    fail "image archive SHA-256 verification failed"
  log "loading verified image archive"
  docker load --input "$downloaded_image" >/dev/null
  docker image inspect "$IMAGE_REF" >/dev/null 2>&1 ||
    fail "archive did not contain expected image ${IMAGE_REF}"
  cleanup_download
  downloaded_image=""
else
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
  log "pulling ${IMAGE_REF}"
  docker pull "$IMAGE_REF"
fi

install -d -m 0700 "$CONFIG_DIR"
container_tls_ca_file=""
if [[ -n "${MASTER_CA_FILE:-}" ]]; then
  [[ "$AGENT_MASTER_URL" == wss://* ]] || fail "MASTER_CA_FILE requires a wss:// AGENT_MASTER_URL"
  [[ -r "$MASTER_CA_FILE" ]] || fail "MASTER_CA_FILE is not readable: $MASTER_CA_FILE"
  install -m 0444 "$MASTER_CA_FILE" "$CONFIG_DIR/master-ca.pem"
  container_tls_ca_file="/run/secrets/master-ca.pem"
  docker_extra_args+=(
    --mount "type=bind,src=${CONFIG_DIR}/master-ca.pem,dst=${container_tls_ca_file},readonly"
  )
fi
install -m 0600 /dev/null "$ENV_FILE"
{
  printf 'AGENT_MASTER_URL=%s\n' "$AGENT_MASTER_URL"
  printf 'AGENT_NAME=%s\n' "$AGENT_NAME"
  printf 'AGENT_SHARED_TOKEN=%s\n' "$AGENT_SHARED_TOKEN"
  printf 'AGENT_MAX_CONCURRENT=%s\n' "$AGENT_MAX_CONCURRENT"
  printf 'AGENT_DEFAULT_TIMEOUT=%s\n' "$AGENT_DEFAULT_TIMEOUT"
  printf 'AGENT_MAX_TIMEOUT=%s\n' "$AGENT_MAX_TIMEOUT"
  printf 'AGENT_RECONNECT_MIN=%s\n' "$AGENT_RECONNECT_MIN"
  printf 'AGENT_RECONNECT_MAX=%s\n' "$AGENT_RECONNECT_MAX"
  printf 'AGENT_HEARTBEAT_INTERVAL=%s\n' "$AGENT_HEARTBEAT_INTERVAL"
  if [[ -n "$container_tls_ca_file" ]]; then
    printf 'AGENT_TLS_CA_FILE=%s\n' "$container_tls_ca_file"
  fi
  if [[ -n "${AGENT_TLS_SERVER_NAME:-}" ]]; then
    printf 'AGENT_TLS_SERVER_NAME=%s\n' "$AGENT_TLS_SERVER_NAME"
  fi
} >"$ENV_FILE"
unset AGENT_SHARED_TOKEN existing_agent_token

if docker container inspect "$ROLLBACK_CONTAINER" >/dev/null 2>&1; then
  rollback_label="$(
    docker container inspect \
      --format '{{ index .Config.Labels "com.situation-awareness.service" }}' \
      "$ROLLBACK_CONTAINER"
  )"
  [[ "$rollback_label" == "agent" || "$ALLOW_LEGACY_CONTAINER_MIGRATION" == "true" ]] ||
    fail "container ${ROLLBACK_CONTAINER} exists but is not managed by this script"
  log "removing the previous rollback container"
  docker container rm --force "$ROLLBACK_CONTAINER" >/dev/null
fi

if docker container inspect "$CONTAINER_NAME" >/dev/null 2>&1; then
  existing_label="$(
    docker container inspect \
      --format '{{ index .Config.Labels "com.situation-awareness.service" }}' \
      "$CONTAINER_NAME"
  )"
  [[ "$existing_label" == "agent" || "$ALLOW_LEGACY_CONTAINER_MIGRATION" == "true" ]] ||
    fail "container ${CONTAINER_NAME} exists but is not managed by this script"
  if [[ "$existing_label" != "agent" ]]; then
    log "migrating legacy container ${CONTAINER_NAME}; it will be retained for rollback"
  fi
  log "preserving existing container as ${ROLLBACK_CONTAINER}"
  docker container rename "$CONTAINER_NAME" "$ROLLBACK_CONTAINER"
  previous_available=true
  docker container stop "$ROLLBACK_CONTAINER" >/dev/null
fi

log "starting ${CONTAINER_NAME}; no host port will be published"
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
  --label "$CONTAINER_LABEL" \
  "${docker_extra_args[@]}" \
  "$IMAGE_REF" >/dev/null

sleep 2
running="$(docker container inspect --format '{{.State.Running}}' "$CONTAINER_NAME")"
if [[ "$running" != "true" ]]; then
  docker container logs --tail 100 "$CONTAINER_NAME" >&2 || true
  fail "Agent container is not running"
fi

registered=false
if [[ "$WAIT_FOR_REGISTRATION" == "true" ]]; then
  log "waiting for Master registration"
  for ((attempt = 1; attempt <= 30; attempt++)); do
    if docker container logs "$CONTAINER_NAME" 2>&1 |
      grep -q 'registered by Master as agent_id='; then
      registered=true
      break
    fi
    sleep 1
  done
fi

log "container is running"
log "Master endpoint: ${AGENT_MASTER_URL}"
log "configuration: ${ENV_FILE}"
log "no Docker port mapping was created"
log "view logs: docker logs --tail 100 ${CONTAINER_NAME}"
log "read the per-node token locally with:"
printf "  sed -n 's/^AGENT_SHARED_TOKEN=//p' %q\n" "$ENV_FILE"

if [[ "$WAIT_FOR_REGISTRATION" == "true" && "$registered" != "true" ]]; then
  printf '\n[deploy] WARNING: Agent is running but Master has not registered it yet.\n'
fi
printf '[deploy] Add or update this node in node_registry_manager with the exact Agent name and token.\n'
printf '[deploy] Ensure this machine can reach the Master IP on TCP %s.\n' "$MASTER_PORT"
if [[ "$previous_available" == "true" ]]; then
  printf '[deploy] Previous container retained as %s for manual rollback.\n' "$ROLLBACK_CONTAINER"
  printf '[deploy] Rollback: docker rm -f %s && docker rename %s %s && docker start %s\n' \
    "$CONTAINER_NAME" "$ROLLBACK_CONTAINER" "$CONTAINER_NAME" "$CONTAINER_NAME"
fi
cleanup_download
trap - ERR
