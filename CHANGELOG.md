# Changelog

All notable changes are listed here. Versions follow [Semantic Versioning](https://semver.org); while
the major version is 0, a minor release may change configuration (each such change is called out).

## [Unreleased]

## [0.1.0] - 2026-10-17

First release.

### Added
- Controller: one ephemeral Railway service per queued GitHub Actions job, with JIT runners for
  `org:` and `repo:` scopes, an HMAC-verified `workflow_job` webhook as a nudge and a poll as the
  truth, `/healthz`, and alerts for jobs queued with no runner coming.
- Reaping of finished runner services about a minute after their job ends, of runners that never
  came online, of idle surplus runners, and of services past `MAX_JOB_MINUTES`.
- `RUNNER_SERVICE_PREFIX`, so two controllers (two labels and images) can share one runner project.
- Runner image (`:vX.Y.Z`): the official actions runner on Ubuntu 24.04 with common build tools and
  shellcheck, without sudo or Docker.
- Postgres variant (`:pg-vX.Y.Z`): PostgreSQL 18, Playwright's Chromium and WebKit libraries and
  browsers, and `ci-postgres` for a per-job database.
- Versioned images on GHCR for every `vX.Y.Z` tag, besides the per-commit sha tags.

### Changed
- The default `RUNNER_LABEL` is `railway` (it was `railway-farzai` before the first release). Set the
  variable explicitly if your workflows use another label.
