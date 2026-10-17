# The runner image: the official actions runner plus the tools workflows expect, with sudo
# and the docker group taken away. It has no docker: a Railway container has no docker daemon.
# The base is pinned by tag and by the digest of its multi-arch index (2.338.0).
ARG RUNNER_VERSION=2.338.0
ARG RUNNER_DIGEST=sha256:4ffadc0002b2581327e06101fc8c06cd189232baf79fe561fac9caeb76f5e807
FROM ghcr.io/actions/actions-runner:${RUNNER_VERSION}@${RUNNER_DIGEST}

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
