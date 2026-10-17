# The runner image: the official actions runner plus the tools workflows expect, with sudo
# and the docker group taken away. It has no docker: a Railway container has no docker daemon.
ARG RUNNER_VERSION=2.338.0
FROM ghcr.io/actions/actions-runner:${RUNNER_VERSION}

USER root
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl wget git git-lfs jq unzip zip xz-utils tar gzip openssh-client \
      build-essential pkg-config python3 python3-pip python3-venv \
 && rm -rf /var/lib/apt/lists/* \
 # a job must not be able to become root or write the runner's own files as root
 && deluser runner sudo \
 && deluser runner docker || true \
 && rm -f /etc/sudoers.d/* \
 && (sed -i '/NOPASSWD/d' /etc/sudoers || true)

COPY entrypoint.sh /entrypoint.sh
RUN chmod 0755 /entrypoint.sh

USER runner
WORKDIR /home/runner
ENV RUNNER_MANUALLY_TRAP_SIG=1 \
    ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1
ENTRYPOINT ["/entrypoint.sh"]
