#!/usr/bin/env bash
set -Eeuo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8080}"
HEALTH_PATH="${HEALTH_PATH:-/healthz}"
CHAT_PATH="${CHAT_PATH:-/api/v1/jobs}"
JOB_EVENTS_PATH_PREFIX="${JOB_EVENTS_PATH_PREFIX:-/api/v1/jobs}"
COUNT="${COUNT:-4}"
MESSAGE="${MESSAGE:-Напиши подробный, но короткий ответ о приватном локальном AI-сервисе.}"
TIMEOUT="${REQUEST_TIMEOUT_SECONDS:-300}"

auth_args=()
if [[ -n "${API_KEY:-}" ]]; then
  auth_args=(-H "X-API-Key: ${API_KEY}")
fi

json_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g; s/\t/\\t/g; s/\r/\\r/g; s/\n/\\n/g'
}

escaped_message="$(json_escape "${MESSAGE}")"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

echo "== health: ${BASE_URL}${HEALTH_PATH}"
curl -fsS --connect-timeout 5 --max-time 15 \
  "${auth_args[@]}" "${BASE_URL}${HEALTH_PATH}" >/dev/null

echo "== submitting ${COUNT} concurrent jobs"
pids=()
for ((i = 1; i <= COUNT; i++)); do
  (
    body="{\"messages\":[{\"role\":\"user\",\"content\":\"${escaped_message} #${i}\"}]}"
    curl -fsS --connect-timeout 5 --max-time 30 \
      "${auth_args[@]}" \
      -H 'Content-Type: application/json' \
      -X POST "${BASE_URL}${CHAT_PATH}" \
      --data "${body}" >"${tmp_dir}/response.${i}"
  ) &
  pids+=("$!")
done

failed=0
for pid in "${pids[@]}"; do
  if ! wait "${pid}"; then
    failed=1
  fi
done
if (( failed )); then
  echo 'Один или несколько запросов создания job завершились ошибкой.' >&2
  exit 1
fi

job_ids=()
for ((i = 1; i <= COUNT; i++)); do
  response="$(cat "${tmp_dir}/response.${i}")"
  job_id="$(printf '%s' "${response}" | sed -nE 's/.*"job_id"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' | head -n 1)"
  if [[ -z "${job_id}" ]]; then
    echo "Ответ ${i} не содержит job_id: ${response}" >&2
    exit 1
  fi
  job_ids+=("${job_id}")
  echo "job ${i}: ${job_id}"
done

echo '== reading SSE streams'
sse_pids=()
for ((i = 1; i <= COUNT; i++)); do
  (
    job_id="${job_ids[$((i - 1))]}"
    output="${tmp_dir}/events.${i}"
    if curl -fsS -N --connect-timeout 5 --max-time "${TIMEOUT}" \
      "${auth_args[@]}" \
      "${BASE_URL}${JOB_EVENTS_PATH_PREFIX}/${job_id}/events" >"${output}"; then
      if grep -Eiq 'event:[[:space:]]*(done|completed)|\[DONE\]|"status"[[:space:]]*:[[:space:]]*"completed"' "${output}"; then
        echo "job ${i}: completed"
      else
        echo "job ${i}: stream ended without completion marker" >&2
        exit 1
      fi
    else
      echo "job ${i}: SSE request failed" >&2
      exit 1
    fi
  ) &
  sse_pids+=("$!")
done

failed=0
for pid in "${sse_pids[@]}"; do
  if ! wait "${pid}"; then
    failed=1
  fi
done

if (( failed )); then
  exit 1
fi

echo "Load test: ${COUNT} конкурентных job отправлены и дочитаны до завершения."
