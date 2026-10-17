# An example derived runner image: the published runner image plus a team's own toolchain, baked in
# at build time so jobs need no sudo. Build it, push it to your registry, and set the controller's
# RUNNER_IMAGE to it. Keep the base's runner user, entrypoint and environment: only add files.
#
#   docker build --platform linux/amd64 -t ghcr.io/<you>/runner-derived:1 examples/derived
#
# The base is pinned by digest; files fetched directly (key, library, CLI, Node, bun) are checked
# against a sha256; the Postgres client is pinned by version, and every apt package (including
# libcurl and Playwright's --with-deps libraries, which take the current version) comes from a signed
# repository;
# pnpm and Playwright are pinned by version through npm, and Playwright fetches its own browsers.
ARG BASE=ghcr.io/farzai/github-runner-railway:v0.1.0@sha256:992d69310adf6e7937f0a101f20f4991feb5f65a766654edfad8404239a14cc9
FROM ${BASE}

USER root

# A specific Postgres client major from PGDG (signing key pinned by sha256), and libcurl headers.
ARG PGDG_KEY_SHA256=0144068502a1eddd2a0280ede10ef607d1ec592ce819940991203941564e8e76
ARG PG_CLIENT_VERSION=17.11-1.pgdg24.04+2
RUN curl -fsSL -o /tmp/pgdg.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc \
 && echo "${PGDG_KEY_SHA256}  /tmp/pgdg.asc" | sha256sum -c - \
 && install -d /usr/share/postgresql-common/pgdg \
 && install -m 0644 /tmp/pgdg.asc /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
 && { echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt noble-pgdg main"; \
      echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt-archive.postgresql.org/pub/repos/apt noble-pgdg-archive main"; } \
      > /etc/apt/sources.list.d/pgdg.list \
 && apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      "postgresql-client-17=${PG_CLIENT_VERSION}" libcurl4-openssl-dev \
 && rm -rf /var/lib/apt/lists/* /tmp/pgdg.asc

# A prebuilt shared library, registered with ldconfig.
ARG DUCKDB_VERSION=v1.5.6
ARG DUCKDB_SHA256=b845005f5132a7d8180057c35e14a7626632258782f871a90861b19c1c03841b
RUN curl -fsSL -o /tmp/libduckdb.zip "https://github.com/duckdb/duckdb/releases/download/${DUCKDB_VERSION}/libduckdb-linux-amd64.zip" \
 && echo "${DUCKDB_SHA256}  /tmp/libduckdb.zip" | sha256sum -c - \
 && unzip -q -o /tmp/libduckdb.zip libduckdb.so duckdb.h -d /tmp/duckdb \
 && install -m 0644 /tmp/duckdb/libduckdb.so /usr/local/lib/ \
 && install -m 0644 /tmp/duckdb/duckdb.h /usr/local/include/ \
 && ldconfig \
 && rm -rf /tmp/libduckdb.zip /tmp/duckdb

# A CLI binary pinned by checksum.
ARG YQ_VERSION=v4.54.1
ARG YQ_SHA256=8e34fc298390875de416e6a4afcb8cabeceb25d9aa8506c1a2f9353cf702ea5f
RUN curl -fsSL -o /usr/local/bin/yq "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_amd64" \
 && echo "${YQ_SHA256}  /usr/local/bin/yq" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/yq

# Node 22 with pnpm, and bun.
ARG NODE_VERSION=22.23.3
ARG NODE_SHA256=df450af89261115ef9f9e3830c3eeb2cc9213b63c720b1af623cb5dcbe2e02de
ARG PNPM_VERSION=12.14.0
ARG BUN_VERSION=bun-v1.4.3
ARG BUN_SHA256=3e730528d89775f03f87f05c06787c4df4099d0a37f3f252e7b829cb23816e18
RUN curl -fsSL -o /tmp/node.tar.xz "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz" \
 && echo "${NODE_SHA256}  /tmp/node.tar.xz" | sha256sum -c - \
 && tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 --exclude='*/CHANGELOG.md' --exclude='*/README.md' \
 && npm install -g "pnpm@${PNPM_VERSION}" \
 && curl -fsSL -o /tmp/bun.zip "https://github.com/oven-sh/bun/releases/download/${BUN_VERSION}/bun-linux-x64.zip" \
 && echo "${BUN_SHA256}  /tmp/bun.zip" | sha256sum -c - \
 && unzip -q -o /tmp/bun.zip -d /tmp \
 && install -m 0755 /tmp/bun-linux-x64/bun /usr/local/bin/bun \
 && rm -rf /tmp/node.tar.xz /tmp/bun.zip /tmp/bun-linux-x64 /root/.npm

# Playwright browsers of a chosen version, with their system libraries. Owned by the runner user so
# a job on another Playwright version can still download its own next to them.
ARG PLAYWRIGHT_VERSION=1.64.1
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
RUN DEBIAN_FRONTEND=noninteractive npx --yes "playwright@${PLAYWRIGHT_VERSION}" install --with-deps chromium webkit \
 && chown -R runner:runner /ms-playwright \
 && rm -rf /root/.npm /var/lib/apt/lists/*

# Back to the base's user, workdir and entrypoint (inherited); fail the build if anything is missing.
USER runner
WORKDIR /home/runner
RUN for t in psql curl-config yq node pnpm bun; do command -v "$t" >/dev/null || { echo "missing $t" >&2; exit 1; }; done \
 && ldconfig -p | grep -q libduckdb \
 && ! sudo -n true 2>/dev/null
