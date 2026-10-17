# The runner image: the official actions runner plus the tools workflows expect, with sudo
# and the docker group taken away. It has no docker: a Railway container has no docker daemon.
# The base is pinned by tag and by the digest of its multi-arch index (2.338.0).
ARG RUNNER_VERSION=2.338.0
ARG RUNNER_DIGEST=sha256:4ffadc0002b2581327e06101fc8c06cd189232baf79fe561fac9caeb76f5e807
FROM ghcr.io/actions/actions-runner:${RUNNER_VERSION}@${RUNNER_DIGEST} AS base

USER root
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl wget git git-lfs jq unzip zip xz-utils tar gzip openssh-client \
      build-essential pkg-config python3 python3-pip python3-venv \
 && rm -rf /var/lib/apt/lists/*

# shellcheck, so actionlint checks run: scripts instead of skipping them silently. Pinned release,
# verified against the digest GitHub publishes for the asset (the image is built for linux/amd64).
ARG SHELLCHECK_VERSION=0.11.0
ARG SHELLCHECK_SHA256=8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198
RUN curl -fsSL -o /tmp/shellcheck.tar.xz \
      "https://github.com/koalaman/shellcheck/releases/download/v${SHELLCHECK_VERSION}/shellcheck-v${SHELLCHECK_VERSION}.linux.x86_64.tar.xz" \
 && echo "${SHELLCHECK_SHA256}  /tmp/shellcheck.tar.xz" | sha256sum -c - \
 && tar -xJf /tmp/shellcheck.tar.xz -C /tmp \
 && install -m 0755 "/tmp/shellcheck-v${SHELLCHECK_VERSION}/shellcheck" /usr/local/bin/shellcheck \
 && rm -rf /tmp/shellcheck.tar.xz "/tmp/shellcheck-v${SHELLCHECK_VERSION}"

# A job must not be able to become root or write the runner's own files as root.
RUN (deluser runner sudo || true) \
 && (deluser runner docker || true) \
 && rm -f /etc/sudoers.d/* \
 && (sed -i '/NOPASSWD/d' /etc/sudoers || true)

# The build fails here, not at the first job, if any of that did not take.
RUN ! id -nG runner | tr ' ' '\n' | grep -qxE 'sudo|docker|root' \
 && ! grep -rqs NOPASSWD /etc/sudoers /etc/sudoers.d \
 && for t in git curl jq unzip python3 gcc make shellcheck; do command -v "$t" >/dev/null || { echo "missing $t" >&2; exit 1; }; done

COPY entrypoint.sh /entrypoint.sh
RUN chmod 0755 /entrypoint.sh

USER runner
WORKDIR /home/runner
ENV RUNNER_MANUALLY_TRAP_SIG=1 \
    ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1
ENTRYPOINT ["/entrypoint.sh"]

# The Postgres variant (target `pg`, label railway-private-pg): the base plus PostgreSQL 18 and the
# system libraries Playwright's Chromium and WebKit need. Nothing runs at boot: a job starts its
# own server with `ci-postgres start` inside its own container, so no database is shared between
# jobs and none outlives one.
FROM base AS pg
USER root
ARG PG_MAJOR=18
# The live repository keeps only recent point releases; the archive keeps every one, so this pin
# still builds after it is pruned.
ARG PG_VERSION=18.6-1.pgdg24.04+2
# sha256 of https://www.postgresql.org/media/keys/ACCC4CF8.asc (PGDG signing key)
ARG PGDG_KEY_SHA256=0144068502a1eddd2a0280ede10ef607d1ec592ce819940991203941564e8e76
ARG NODE_VERSION=22.23.3
ARG NODE_SHA256=df450af89261115ef9f9e3830c3eeb2cc9213b63c720b1af623cb5dcbe2e02de
ARG PLAYWRIGHT_VERSION=1.63.0
RUN curl -fsSL -o /tmp/pgdg.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc \
 && echo "${PGDG_KEY_SHA256}  /tmp/pgdg.asc" | sha256sum -c - \
 && install -d /usr/share/postgresql-common/pgdg \
 && install -m 0644 /tmp/pgdg.asc /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
 && echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt noble-pgdg main" \
      > /etc/apt/sources.list.d/pgdg.list \
 && echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt-archive.postgresql.org/pub/repos/apt noble-pgdg-archive main" \
      >> /etc/apt/sources.list.d/pgdg.list \
 && install -d /etc/postgresql-common \
 && echo "create_main_cluster = false" > /etc/postgresql-common/createcluster.conf \
 && apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      "postgresql-${PG_MAJOR}=${PG_VERSION}" "postgresql-client-${PG_MAJOR}=${PG_VERSION}" \
 && rm -f /tmp/pgdg.asc
# Playwright's own list of system libraries for Chromium and WebKit, and its browsers, installed
# with a pinned Node and Playwright that are removed again: jobs bring their own Node
# (actions/setup-node).
RUN curl -fsSL -o /tmp/node.tar.xz "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz" \
 && echo "${NODE_SHA256}  /tmp/node.tar.xz" | sha256sum -c - \
 && install -d /tmp/node && tar -xJf /tmp/node.tar.xz -C /tmp/node --strip-components=1 \
 && PATH="/tmp/node/bin:${PATH}" DEBIAN_FRONTEND=noninteractive \
      /tmp/node/bin/npx --yes "playwright@${PLAYWRIGHT_VERSION}" install-deps chromium webkit \
 && PATH="/tmp/node/bin:${PATH}" PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
      /tmp/node/bin/npx --yes "playwright@${PLAYWRIGHT_VERSION}" install chromium webkit ffmpeg \
 && chown -R runner:runner /ms-playwright \
 && rm -rf /tmp/node /tmp/node.tar.xz /root/.npm /var/lib/apt/lists/*
# The browsers of that Playwright version are baked in, so a job's `playwright install chromium
# webkit` finds them and downloads nothing. The directory belongs to the runner user: a job whose
# Playwright is newer than the image still downloads its own browsers there (one container, one job).
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
COPY ci-postgres /usr/local/bin/ci-postgres
RUN chmod 0755 /usr/local/bin/ci-postgres \
 && ln -s "/usr/lib/postgresql/${PG_MAJOR}/bin/initdb" /usr/local/bin/initdb \
 && ln -s "/usr/lib/postgresql/${PG_MAJOR}/bin/pg_ctl" /usr/local/bin/pg_ctl \
 && ! id -nG runner | tr ' ' '\n' | grep -qxE 'sudo|docker|root' \
 && for t in initdb pg_ctl psql ci-postgres; do command -v "$t" >/dev/null || { echo "missing $t" >&2; exit 1; }; done
USER runner
WORKDIR /home/runner

# The default target stays the plain runner image.
FROM base
