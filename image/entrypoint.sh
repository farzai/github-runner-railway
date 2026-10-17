#!/bin/bash
# Runs exactly one job: the runner registers with the JIT config from the environment, takes
# one job and exits. Railway starts this container once per job and never restarts it.
set -euo pipefail

if [ -z "${ACTIONS_RUNNER_INPUT_JITCONFIG:-}" ]; then
  echo "entrypoint: ACTIONS_RUNNER_INPUT_JITCONFIG is not set; nothing to run" >&2
  exit 2
fi

# Never let a job find Railway's or the controller's variables here: this container was
# given the JIT config and nothing else, but drop the platform's own ones all the same.
for v in $(env | grep -E '^RAILWAY_' | cut -d= -f1); do unset "$v"; done

cd /home/runner
exec ./run.sh
