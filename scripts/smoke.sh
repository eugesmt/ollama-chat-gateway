#!/usr/bin/env bash
set -Eeuo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8080}"
HEALTH_PATH="${HEALTH_PATH:-/healthz}"
CHAT_PATH="${CHAT_PATH:-/api/v1/jobs}"
JOB_EVENTS_PATH_PREFIX="${JOB_EVENTS_PATH_PREFIX:-/api/v1/jobs}"
MODEL="${OLLAMA_MODEL:-}"
MESSAGE="${MESSAGE:-Ответь одним коротким предложением: сервис доступен?}"
TIMEOUT="${REQUEST_TIMEOUT_SECONDS:-180}"

auth_args=()
if [[ -n "${API_KEY:-}" ]]; then
  auth_args=(-H "X-API-Key: ${API_KEY}")
fi

echo "== health: ${BASE_URL}${HEALTH_PATH}"
curl -fsS --connect-timeout 5 --max-time 15 \
  "${auth_args[@]}" "${BASE_URL}${HEALTH_PATH}"
printf '\n'

json_escape() {
  # Messages used by this script are normally plain text. Escape the JSON
  # characters needed for a safe request body without requiring jq.
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g; s/\t/\\t/g; s/\r/\\r/g; s/\n/\\n/g'
}

escaped_message="$(json_escape "${MESSAGE}")"
body="{\"messages\":[{\"role\":\"user\",\"content\":\"${escaped_message}\"}]"
if [[ -n "${MODEL}" ]]; then
  body="${body},\"model\":\"$(json_escape "${MODEL}")\""
fi
body="${body}}"

echo "== create job: ${BASE_URL}${CHAT_PATH}"
response="$(
  curl -fsS --connect-timeout 5 --max-time 30 \
    "${auth_args[@]}" \
    -H 'Content-Type: application/json' \
    -X POST "${BASE_URL}${CHAT_PATH}" \
    --data "${body}"
)"
printf '%s\n' "${response}"

job_id="$(printf '%s' "${response}" | sed -nE 's/.*"job_id"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' | head -n 1)"
if [[ -z "${job_id}" ]]; then
  echo 'Не найден job_id в ответе create job.' >&2
  exit 1
fi

events_url="${BASE_URL}${JOB_EVENTS_PATH_PREFIX}/${job_id}/events"
events_file="$(mktemp)"
trap 'rm -f "${events_file}"' EXIT

echo "== SSE: ${events_url}"
curl -fsS -N --connect-timeout 5 --max-time "${TIMEOUT}" \
  "${auth_args[@]}" "${events_url}" | tee "${events_file}"

if ! grep -Eiq 'event:[[:space:]]*(done|completed)|\[DONE\]|"status"[[:space:]]*:[[:space:]]*"completed"' "${events_file}"; then
  echo 'SSE не содержит признака завершения job.' >&2
  exit 1
fi

echo 'Smoke test: запрос создан, SSE прочитан до завершения.'
