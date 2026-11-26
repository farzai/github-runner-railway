# Changelog

All notable changes are listed here. Versions follow [Semantic Versioning](https://semver.org); while
the major version is 0, a minor release may change configuration (each such change is called out).

## [Unreleased]

### Added
- `RUNNER_POOLS`: one controller serves several runner images. It takes a JSON list of pools, each with a `label`, `image`, `prefix`, `max_runners`, `scopes` and an optional `region` (falling back to `RUNNER_REGION`). Each cycle reads every scope once, whichever pools list it, gives each queued job to the pool whose label it asks for, and plans each pool on its own, so a full pool never holds up another. The App's one webhook wakes the controller for a job of any pool, so a second image no longer needs a second controller that only polls and scans the same repositories again. Labels and prefixes must differ across pools. The single-pool variables (`RUNNER_IMAGE`, `RUNNER_SCOPES`, `RUNNER_LABEL`, `RUNNER_SERVICE_PREFIX`, `MAX_RUNNERS`) still configure one pool unchanged; setting them together with `RUNNER_POOLS` is refused. `MAX_CREATE_PER_CYCLE` applies per pool. docs/deploy.md describes moving from two controllers to one; the second-controller setup is no longer documented.
- The `sqlite3` command-line shell in every image variant, for tests and scripts that drive a SQLite database from the shell.
- `libssl-dev` in every image variant, so Rust crates that link OpenSSL (`openssl-sys`) build on the pool; the build checks that `pkg-config` finds it.
- The GitHub CLI (`gh` 2.102.0, pinned by version and sha256) in every image variant, so a job can open or comment on issues without a GitHub-hosted runner. Nothing is logged in: a job passes `GH_TOKEN`.

### Fixed
- A queued job whose run GitHub's `runs?status=queued` list leaves out gets a runner. GitHub left such a run out for 25 minutes on 2026-11-21 and its job never got a runner. Each cycle now also reads the 20 newest runs of each repository without the status filter, and serves the queued jobs of any run that is not completed and was not already listed. `/healthz` reports `unlisted_jobs`, the `cycle` log line carries `unlisted`, and each such job is logged at warn. If the extra read fails (a 5xx, or a 403 at the rate limit), the jobs from the filtered lists are still served: the failure is logged at warn with the repository and status, counted as `unlisted_read_failures` on `/healthz` and `unlisted_read_failed` on the `cycle` line, and does not fail the scope. On an idle repository the extra read is an unchanged `304`, which does not count against the rate limit.
- A scope in `RUNNER_SCOPES` that GitHub answers 404 for (a deleted repository, for example) no longer fails every cycle and turns `/healthz` 503: it is counted in `unreadable_scopes`, logged at warn once a cycle and left out of the failing alert, while the other scopes keep getting runners. After `ALERT_FAILED_CYCLES` cycles in a row such a scope raises one alert of its own, and one more when it reads again. When every scope answers 404 (the App uninstalled, for example), the cycle still fails. In an org scope, a repository or run that vanishes mid-cycle is skipped instead of hiding the whole org. Other GitHub errors still fail the cycle.
- `/healthz` answers at once while a cycle runs, with the previous cycle's status; it waited for the cycle to end (about a minute with many repositories) and health checks timed out.
- Jobs queued in a run that GitHub reports as `pending` (for example while it waits on a concurrency group) are served; the controller listed only `queued` and `in_progress` runs and left such jobs waiting forever.

### Changed
- The webhook matches a job's labels without case, as GitHub and the job scan already do; a label written `Railway` in `runs-on` now wakes the controller too.
- A label of `self-hosted`, `linux` or `x64` is refused at start: every runner has those, so they cannot pick out a pool.
- A queued-job webhook starts a cycle after a 3 s debounce instead of a fixed 20 s; cycles still start at least 20 s apart (measured from the previous cycle's start).

- CI rebuilds the runner images only when `image/` changes and the controller image only when its Go code or Dockerfile changes, with a per-target layer cache; a version tag still publishes all three.

### Added
- A `php` image variant (`php-<version>`): PHP 8.3, 8.4 and 8.5 from the ondrej/php PPA and Composer, with `use-php <version>` to choose one without root. `shivammathur/setup-php` cannot run on the default images (it needs `sudo` and then waits instead of failing); the README lists which `setup-*` actions work.
- `RUNNER_REGION` (optional): the Railway region a controller creates its runners in, set per controller through `multiRegionConfig`. Unset keeps today's placement in the workspace's preferred region.
- `ALERT_FAILED_CYCLES` (default 3): after that many failed cycles in a row the controller alerts once (naming scopes unreadable that long), `/healthz` answers 503, and one "recovered" alert follows the next good cycle. `/healthz` also reports `last_success`, `consecutive_failures` and `unreadable_scopes`, the number of scopes the last cycle could not read. Since `/healthz` is public, it never names them (a private repository's name would leak); the names are in the log lines and the failing alert.

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
