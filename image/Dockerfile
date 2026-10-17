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

# A job must not be able to become root or write the runner's own files as root.
RUN (deluser runner sudo || true) \
 && (deluser runner docker || true) \
 && rm -f /etc/sudoers.d/* \
 && (sed -i '/NOPASSWD/d' /etc/sudoers || true)

# The build fails here, not at the first job, if any of that did not take.
RUN ! id -nG runner | tr ' ' '\n' | grep -qxE 'sudo|docker|root' \
 && ! grep -rqs NOPASSWD /etc/sudoers /etc/sudoers.d \
 && for t in git curl jq unzip python3 gcc make; do command -v "$t" >/dev/null || { echo "missing $t" >&2; exit 1; }; done

COPY entrypoint.sh /entrypoint.sh
RUN chmod 0755 /entrypoint.sh

USER runner
WORKDIR /home/runner
ENV RUNNER_MANUALLY_TRAP_SIG=1 \
    ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1
ENTRYPOINT ["/entrypoint.sh"]
