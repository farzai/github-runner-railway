# An example derived runner image for PHP projects. shivammathur/setup-php cannot run on the default
# images: it calls `sudo` for every step (apt, ln, tee), which the default images do not give the
# runner user, and it then waits instead of failing (farzai/github-runner-railway#38). So PHP is
# baked in at build time and a job picks a version with `use-php`, with no setup-php step.
#
#   docker build --platform linux/amd64 -t ghcr.io/<you>/runner-php:1 examples/php
#
# In a workflow:   - run: use-php 8.3        # puts that version first on PATH for the later steps
#                  - run: composer install
#
# Without `use-php`, `php` is the apt alternatives default (the newest installed version).
#
# The base is pinned by digest; PHP comes from the ondrej/php PPA (key pinned by fingerprint),
# Composer from its release, pinned by version and sha256.
ARG BASE=ghcr.io/farzai/github-runner-railway:v0.1.0@sha256:992d69310adf6e7937f0a101f20f4991feb5f65a766654edfad8404239a14cc9
FROM ${BASE}

USER root

# Space-separated PHP versions and the extensions installed for each (php<ver>-<ext> packages).
ARG PHP_VERSIONS="8.3 8.4 8.5"
ARG PHP_EXTENSIONS="cli mbstring zip curl xml sqlite3 intl bcmath"
# Fingerprint of the ondrej/php PPA signing key, fetched from the Ubuntu keyserver and checked by
# gpg against it
ARG PPA_FPR=B8DC7E53946656EFBCE4C1DD71DAEAAB4AD4CAB6
ARG COMPOSER_VERSION=2.10.3
ARG COMPOSER_SHA256=7a2d379d5b8ffdaa028580ef26494c36d2feef4b178d3dd1473a4dbc5e17c8d6

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends gnupg \
 && gpg --batch --keyserver hkps://keyserver.ubuntu.com --recv-keys "${PPA_FPR}" \
 && gpg --batch --export "${PPA_FPR}" > /usr/share/keyrings/ondrej-php.gpg \
 && echo "deb [signed-by=/usr/share/keyrings/ondrej-php.gpg] https://ppa.launchpadcontent.net/ondrej/php/ubuntu noble main" \
      > /etc/apt/sources.list.d/ondrej-php.list \
 && apt-get update \
 && for v in ${PHP_VERSIONS}; do \
      pkgs=""; for e in ${PHP_EXTENSIONS}; do pkgs="${pkgs} php${v}-${e}"; done; \
      DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ${pkgs}; \
    done \
 && rm -rf /var/lib/apt/lists/*

# Composer, from the release at getcomposer.org pinned by version and sha256 (the value is
# https://getcomposer.org/download/<version>/composer.phar.sha256sum; the build fails if it differs).
RUN test -n "${COMPOSER_SHA256}" \
 && curl -fsSL -o /usr/local/bin/composer "https://getcomposer.org/download/${COMPOSER_VERSION}/composer.phar" \
 && echo "${COMPOSER_SHA256}  /usr/local/bin/composer" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/composer

# use-php <version>: first on PATH for the following steps of the job, with no root. php, phpize and
# friends resolve to that version through a shim directory in the job's temp dir.
COPY use-php /usr/local/bin/use-php
RUN chmod 0755 /usr/local/bin/use-php \
 && for v in ${PHP_VERSIONS}; do php"${v}" -v >/dev/null; done

USER runner
WORKDIR /home/runner
