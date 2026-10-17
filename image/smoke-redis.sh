#!/bin/bash
# Smoke test of a Redis-enabled image, run inside it as the runner user: start ci-redis, SET and
# GET through CI_REDIS_URL, stop it.
set -euo pipefail
RUNNER_TEMP="$(mktemp -d)"
GITHUB_ENV="$(mktemp)"
export RUNNER_TEMP GITHUB_ENV
ci-redis start >/dev/null
# shellcheck disable=SC1090 # the env file ci-redis wrote
source <(sed 's/^/export /' "${GITHUB_ENV}")
redis-cli -u "${CI_REDIS_URL}" SET smoke ok >/dev/null
got="$(redis-cli -u "${CI_REDIS_URL}" GET smoke)"
if [[ "${got}" != ok ]]; then
  echo "GET smoke = ${got}, want ok" >&2
  exit 1
fi
ci-redis stop >/dev/null
if redis-cli -u "${CI_REDIS_URL}" PING >/dev/null 2>&1; then
  echo "server still answers after stop" >&2
  exit 1
fi
echo "ok: $(redis-server --version | awk '{print $1, $3}') on ${CI_REDIS_URL}"
