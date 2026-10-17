#!/bin/bash
# Installs a pinned redis-server and redis-cli from packages.redis.io (signing key pinned by sha256)
# and ci-redis. Used by the redis target and by the pg target when REDIS_VERSION is set.
set -euo pipefail
: "${REDIS_VERSION:?REDIS_VERSION is the packages.redis.io version, e.g. 6:8.10.2-1rl1~noble1}"
: "${REDIS_KEY_SHA256:?}"
curl -fsSL -o /tmp/redis.gpg https://packages.redis.io/gpg
echo "${REDIS_KEY_SHA256}  /tmp/redis.gpg" | sha256sum -c -
gpg --dearmor -o /usr/share/keyrings/redis-archive-keyring.gpg /tmp/redis.gpg
echo "deb [signed-by=/usr/share/keyrings/redis-archive-keyring.gpg] https://packages.redis.io/deb noble main" \
  > /etc/apt/sources.list.d/redis.list
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
  "redis-server=${REDIS_VERSION}" "redis-tools=${REDIS_VERSION}"
rm -rf /var/lib/apt/lists/* /tmp/redis.gpg
command -v redis-server >/dev/null && command -v redis-cli >/dev/null
