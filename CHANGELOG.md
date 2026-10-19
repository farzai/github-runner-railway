# Changelog

All notable changes are listed here. Versions follow [Semantic Versioning](https://semver.org); while
the major version is 0, a minor release may change configuration (each such change is called out).

## [Unreleased]

### Added
- The GitHub CLI (`gh` 2.102.0, pinned by version and sha256) in every image variant, so a job can open or comment on issues without a GitHub-hosted runner. Nothing is logged in: a job passes `GH_TOKEN`.

### Fixed
- Jobs queued in a run that GitHub reports as `pending` (for example while it waits on a concurrency group) are served; the controller listed only `queued` and `in_progress` runs and left such jobs waiting forever.

### Changed
- A queued-job webhook starts a cycle after a 3 s debounce instead of a fixed 20 s; cycles still start at least 20 s apart (measured from the previous cycle's start).

- CI rebuilds the runner images only when `image/` changes and the controller image only when its Go code or Dockerfile changes, with a per-target layer cache; a version tag still publishes all three.

### Added
- A `php` image variant (`php-<version>`): PHP 8.3, 8.4 and 8.5 from the ondrej/php PPA and Composer, with `use-php <version>` to choose one without root. `shivammathur/setup-php` cannot run on the default images (it needs `sudo` and then waits instead of failing); the README lists which `setup-*` actions work.
- `RUNNER_REGION` (optional): the Railway region a controller creates its runners in, set per controller through `multiRegionConfig`. Unset keeps today's placement in the workspace's preferred region.
- `ALERT_FAILED_CYCLES` (default 3): after that many failed cycles in a row the controller alerts once (naming scopes unreadable that long), `/healthz` answers 503, and one "recovered" alert follows the next good cycle. `/healthz` also reports `last_success`, `consecutive_failures` and `unreadable_scopes`.

### Fixed
- A controller whose every cycle failed kept answering 200 on `/healthz` and raised no alert.

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
