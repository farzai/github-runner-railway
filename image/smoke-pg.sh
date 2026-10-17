#!/bin/bash
# Smoke test of a pg image, run inside it as the runner user (CI runs it for each built variant):
#   smoke-pg.sh <expected major> [extension ...]
# Starts a server with ci-postgres, checks the major version, and creates each extension.
set -euo pipefail
want_major="${1:?usage: smoke-pg.sh <major> [extension ...]}"
shift
RUNNER_TEMP="$(mktemp -d)"
GITHUB_ENV="$(mktemp)"
export RUNNER_TEMP GITHUB_ENV
ci-postgres start smoke smoke smoke >/dev/null
# shellcheck disable=SC1090 # the env file ci-postgres wrote
source <(sed 's/^/export /' "${GITHUB_ENV}")
major="$(psql "${CI_POSTGRES_URL}" -tAc 'show server_version_num' | cut -c1-2)"
if [[ "${major}" != "${want_major}" ]]; then
  echo "server major ${major}, want ${want_major}" >&2
  exit 1
fi
for ext in "$@"; do
  psql "${CI_POSTGRES_URL}" -v ON_ERROR_STOP=1 -qc "CREATE SCHEMA IF NOT EXISTS ext_${ext}; CREATE EXTENSION ${ext} SCHEMA ext_${ext};" >/dev/null
  echo "extension ${ext}: $(psql "${CI_POSTGRES_URL}" -tAc "select extversion from pg_extension where extname = '${ext}'")"
done
ci-postgres stop >/dev/null
echo "ok: PostgreSQL ${major}$( (($#)) && echo " with $*")"
