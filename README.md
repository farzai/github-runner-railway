# github-runner-railway

Autoscaling, ephemeral self-hosted GitHub Actions runners on [Railway](https://railway.com):
**one short-lived Railway service per job**. A small controller watches GitHub for queued jobs with
your label, registers a one-job (JIT) runner for each, starts it as its own Railway service, and
deletes the service when the job is over. Nothing is shared between two jobs.

```
GitHub App webhook (workflow_job)  -->  controller  -->  Railway API
                                          |   ^             |
                 poll queued jobs/runners |   |             v
                                          v   |      runner service (one per job)
                                        GitHub          run.sh --jitconfig  ->  one job  ->  exit
```

It is meant for **private repositories** of one owner or team. Read [Limits](#limits) and
[SECURITY.md](SECURITY.md) before you point it at anything else.

## How it works

* **One service per job.** A fresh container per job, so nothing a job leaves behind (processes,
  files, caches) reaches the next one.
* **Safe scale-down.** The controller deletes exactly the service it picked, and only after GitHub
  agreed the runner is idle (GitHub refuses to delete a busy runner). Services never restart.
* **Org and personal repositories.** `org:<name>` scopes get org runners in a dedicated runner group;
  `repo:<owner>/<name>` scopes get repository-scoped runners, which is what personal accounts need.
* **Webhook plus poll.** The webhook only wakes the controller; every `POLL_SECONDS` it reads GitHub
  and Railway afresh and reconciles: create a runner for every queued job no runner is coming for,
  remove finished, idle-surplus or overdue services.

| Path | What |
| --- | --- |
| `cmd/controller` | the controller binary (HTTP: `/webhook`, `/healthz`) |
| `internal/reconcile` | the pure planner: what to create and what to remove |
| `internal/ghapp` | GitHub App auth, queued jobs, runners, JIT configs (standard library only) |
| `internal/railway` | create, list and delete runner services |
| `internal/webhook` | HMAC-verified `workflow_job` receiver |
| `image/` | the runner image (`base`) and its Postgres + Playwright variant (`pg`) |

## Quick start

You need a GitHub organization or account you administer, a Railway account (a plan with enough
CPU and memory for your jobs; usage is billed), and the repositories you want to serve.

### 1. Two Railway projects

* **Controller project**: one service, the controller. It holds the GitHub App key and the Railway
  token.
* **Runner project**: nothing but the runner services the controller creates and deletes.

They are separate because services in one Railway environment share a private network: a job must
not be able to reach the controller. In the runner project create a **project token** (Project
Settings, Tokens) and note the project id and environment id.

### 2. The GitHub App

Create it by hand with exactly the settings below, or from the manifest in
[`docs/github-app-manifest.json`](docs/github-app-manifest.json) with GitHub's
[app manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest):

1. Edit the manifest: a unique `name`, your controller's webhook URL in `hook_attributes.url`, and a
   `redirect_url` you control. If you serve only `repo:` scopes, delete
   `organization_self_hosted_runners`.
2. Submit it as the `manifest` field of an HTML form POST to
   `https://github.com/settings/apps/new` (personal account) or
   `https://github.com/organizations/<org>/settings/apps/new`, and confirm on GitHub.
3. GitHub redirects to `redirect_url?code=<code>`. Within an hour, exchange the code:
   `curl -X POST https://api.github.com/app-manifests/<code>/conversions`. The answer holds the App
   `id`, the private key (`pem`) and the `webhook_secret`: put them straight into the controller's
   variables and do not keep them anywhere else.

The settings:

* Repository permissions: **Administration: Read and write** (to create and delete repository
  runners), **Actions: Read**, **Metadata: Read**.
* Organization permissions: **Self-hosted runners: Read and write** (only for `org:` scopes; leave it
  out if you serve only `repo:` scopes).
* Subscribe to the **Workflow job** event. Webhook URL: `https://<controller domain>/webhook`, with a
  webhook secret you generate.

Generate a private key, then install the App on the repositories (or organization) you serve.
Administration write is broad: install it on the repositories you serve when you can.

For an `org:` scope, create a dedicated runner group (Settings, Actions, Runner groups) limited to
the repositories you serve, with "Allow public repositories" off, and note its id.

### 3. Deploy the controller

In the controller project, add a service from the image
`ghcr.io/farzai/github-runner-railway-controller:<version>` (see [Versions](#versions)), give it a
public domain, port `8080`, healthcheck path `/healthz`, and the variables below. Put the domain into
the App's webhook URL.

```
GITHUB_APP_ID=123456
GITHUB_APP_PRIVATE_KEY=-----BEGIN RSA PRIVATE KEY-----\n...
GITHUB_WEBHOOK_SECRET=<the App's webhook secret>
RUNNER_RAILWAY_TOKEN=<runner project token>
RUNNER_RAILWAY_PROJECT_ID=<runner project id>
RUNNER_RAILWAY_ENVIRONMENT_ID=<runner environment id>
RUNNER_IMAGE=ghcr.io/farzai/github-runner-railway:<version>
RUNNER_SCOPES=repo:acme/app,repo:acme/api
RUNNER_LABEL=railway
MAX_RUNNERS=2
```

`/healthz` answers 200 once a cycle has completed, and 503 when the last cycle is older than three polls or `ALERT_FAILED_CYCLES` cycles in a row have failed. Its JSON: `last_cycle`, `last_success`, `consecutive_failures`, `unreadable_scopes`, `services`, `queued`, `error`. `github_rate_limits` shows the requests GitHub last reported left per installation account (a warning is logged once per window below a tenth). The log line `controller up` shows the scopes,
label, prefix, image and limits it runs with.

### 4. Use it in a workflow

```yaml
jobs:
  test:
    runs-on: [self-hosted, railway]
```

Always name your label: the runners also carry `self-hosted`, `linux` and `x64`, and a job that asks
for a label the runners do not have is not served. If you lint workflows with actionlint, declare the
label in `.github/actionlint.yaml`:

```yaml
self-hosted-runner:
  labels:
    - railway
```

Start with one repository and `MAX_RUNNERS=1`, watch a job go through, then add scopes and raise the
limits. [docs/deploy.md](docs/deploy.md) has the full guide, rollout and rollback.

## Configuration

| Variable | Meaning | Default |
| --- | --- | --- |
| `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET` | the App (the PEM may keep `\n` escaped) | required |
| `RUNNER_RAILWAY_TOKEN`, `RUNNER_RAILWAY_PROJECT_ID`, `RUNNER_RAILWAY_ENVIRONMENT_ID` | a project token, project and environment of the **runner** project (not `RAILWAY_*`: Railway injects those for the controller's own project, and the controller refuses to manage the project it runs in) | required |
| `RUNNER_IMAGE` | the runner image, pinned to a version or digest | required |
| `RUNNER_SCOPES` | comma list of `org:<name>` and `repo:<owner>/<name>` that may get runners | required |
| `RUNNER_LABEL` | the `runs-on` label of this controller's runners | `railway` |
| `RUNNER_SERVICE_PREFIX` | Railway service-name prefix of this controller's runners; a second controller in the same runner project needs its own (a letter, a letter or digit, a dash, e.g. `rp-`) | `rr-` |
| `RUNNER_GROUP_ID` | the dedicated runner group of org runners; required for an `org:` scope, and the Default group (1) is refused | none |
| `MAX_RUNNERS` / `MAX_CREATE_PER_CYCLE` | services in all / created in one cycle | `8` / `4` |
| `POLL_SECONDS` | polling period (a queued-job webhook wakes a cycle sooner: after a 3 s debounce, and at least 20 s after the previous cycle started) | `120` |
| `STARTUP_GRACE_SECONDS` | how long a registered runner may stay offline before its service is removed | `300` |
| `IDLE_TTL_SECONDS` | how long a surplus idle runner is kept | `120` |
| `MAX_JOB_MINUTES` | job timeout; a service older than this plus 10 minutes is removed | `360` |
| `ALERT_QUEUED_MINUTES` / `ALERT_WEBHOOK_URL` | log an alert (and POST `{"text": ...}` to the URL) when a job waits this long with no runner coming | `5` / none |
| `ALERT_FAILED_CYCLES` | after this many failed cycles in a row: one alert (naming scopes unreadable that long), `/healthz` 503, and one "recovered" alert when a cycle succeeds again | `3` |
| `PORT` | HTTP port | `8080` |

## The runner images

| Image | Contents |
| --- | --- |
| `ghcr.io/farzai/github-runner-railway:<version>` | the official `actions-runner` (Ubuntu 24.04) plus git, curl, jq, python3, build-essential, shellcheck; no `sudo`, no Docker |
| `ghcr.io/farzai/github-runner-railway:pg-<version>` | the above plus PostgreSQL 18, Playwright's Chromium/WebKit system libraries and browsers (see `image/Dockerfile` for the pinned versions), and `ci-postgres` |
| `ghcr.io/farzai/github-runner-railway:sudo-<version>` | the base image plus passwordless `sudo` for the runner user (opt-in, see below) |
| `ghcr.io/farzai/github-runner-railway:redis-<version>` | the base image plus a pinned `redis-server` and `ci-redis` |

To serve both, run a **second controller** with `RUNNER_LABEL=railway-pg`, `RUNNER_SERVICE_PREFIX=rp-`
and the `pg-` image, in the same runner project. Each controller lists and removes only its own
services and only takes jobs that ask for its label. A GitHub App has one webhook URL, so the second
controller works on polling alone (set `POLL_SECONDS=60`).

The published `pg-` image has PostgreSQL 18 and no extensions. Build your own with another major and
PGDG extensions (all pinned, from the signed PGDG repository) and push it to your registry:

```
docker build --platform linux/amd64 --target pg \
  --build-arg PG_MAJOR=16 --build-arg PG_VERSION=16.15-1.pgdg24.04+2 \
  --build-arg "PG_EXTENSIONS=postgresql-16-partman=5.5.0-1.pgdg24.04+1" \
  -t ghcr.io/<you>/runner-pg16:1 image/
```

`PG_MAJOR` (16, 17 or 18), `PG_VERSION` (that major's exact PGDG package version), and
`PG_EXTENSIONS` (space-separated `postgresql-<major>-<name>=<version>`; anything else fails the build).
Find versions with `apt-cache madison` against PGDG, or in its package index. `image/smoke-pg.sh
<major> [extension ...]` checks a built image (CI runs it for 18 and for 16 with pg_partman).

In a job on the `pg` image, start a throwaway database inside the job's own container:

```yaml
runs-on: [self-hosted, railway-pg]
steps:
  - run: ci-postgres start app secret app_test   # user, password, database
  - run: npm test                                 # CI_POSTGRES_URL is set from here on
```

`ci-postgres` runs initdb as the runner user, listens on 127.0.0.1 only on a free port with fsync
off, masks the password, and writes `CI_POSTGRES_URL`, `CI_POSTGRES_PORT` and `CI_POSTGRES_DATA` to
`$GITHUB_ENV`. The server ends with the container.

### Redis

In a job on the `redis-` image (or a `pg` image built with `--build-arg REDIS_VERSION=<version>`, for
both), start a throwaway Redis the same way as Postgres:

```yaml
steps:
  - run: ci-redis start          # optional port; CI_REDIS_URL is set from the next step on
  - run: npm test
```

`ci-redis` runs `redis-server` as the runner user on 127.0.0.1 only (a free port unless one is given),
without persistence, waits until it answers `PING`, and writes `CI_REDIS_URL`, `CI_REDIS_PORT` and
`CI_REDIS_DIR` to `$GITHUB_ENV`; `ci-redis stop` shuts it down. Redis comes from packages.redis.io,
with the signing key pinned by sha256 and the version pinned in `image/Dockerfile`.

### Your own image

Build a derived image `FROM ghcr.io/farzai/github-runner-railway:<version>` and add what your jobs
need at build time, so they need no sudo. [`examples/derived/Dockerfile`](examples/derived/Dockerfile)
pins its base by digest and adds a Postgres client of a chosen major, libcurl headers, a prebuilt shared library registered with
`ldconfig`, a CLI pinned by checksum, Node 22 with pnpm and bun, and Playwright browsers with their
system libraries; CI builds it on every change. Switch to `root` for the installs and end with
`USER runner` and `WORKDIR /home/runner`; keep the base's entrypoint (it reads
`ACTIONS_RUNNER_INPUT_JITCONFIG`, runs one job and exits). Push it to your registry and set
`RUNNER_IMAGE` to it.

### The sudo variant (opt-in)

For workflows you cannot change yet (`sudo apt-get install`, `playwright install --with-deps`,
`ldconfig` in a step), the `sudo-<version>` image gives the runner user passwordless sudo. Each job
is its own container, so root stays inside that job's container and is gone when it ends, but the
job can now change anything in it, and the container still shares the runner project's private
network with concurrent jobs. Use it for private repositories only, in its own pool (a controller
with its own label and `RUNNER_SERVICE_PREFIX`), and prefer a derived image where you can. The
default images stay sudo-free.

## Limits

* **Not for public repositories.** Runner services share a Railway private network, so a job from a
  public repository (anyone can open a pull request) could reach a concurrent private job. Serve
  private repositories only, and require approval for fork pull request workflows (SECURITY.md).
* **No Docker** (a Railway container has no daemon): no `docker build`, `docker://` actions, job
  `container:` or `services:`. Use the `pg` image or a hosted runner for those.
* **No sudo** in the default images, so `sudo apt-get install` fails: use `setup-*` actions, a derived
  image, or the opt-in sudo variant.
* **CPU and processes:** each container is capped by your Railway plan (for example 24 vCPU, 24 GB and
  about 1,000 processes and threads). `nproc` shows the host's cores, not the quota; Node's
  `os.availableParallelism()` reads the quota. Pass explicit `-j`/worker counts to be safe.
* **Railway API:** 1,000 requests an hour per token. A job costs about four calls and a cycle one, so
  roughly 230 jobs an hour at the default poll; two controllers sharing one token share that budget.
* **GitHub API:** 5,000 requests an hour per installation, shared by every scope of that installation
  (one access token per installation). Run and job lists are read with `If-None-Match`, and an
  unchanged list (`304`) does not count against the limit. Prefer `repo:` scopes for large organizations.
* **Cold start** is the Railway deploy plus the image pull: about 20 s when the host has the image,
  longer on a host's first pull of a large image (see `docs/spike.md`).

## Cost

Railway bills the CPU, memory and network the runner services actually use, plus a little for the
controller, which idles most of the time. A job costs roughly its duration times the CPU and memory it
uses; check your plan's prices. Keep `MAX_RUNNERS` and `MAX_JOB_MINUTES` as the real cap on spend.
Railway's usage limits are set per **workspace**, not per project, and reaching the hard limit takes
*every* workload in the workspace offline (Railway docs, usage limits, read 2026-10-17). Set a soft
limit (an email alert) for the runner fleet, and a hard limit only if the workspace holds nothing
else you need running.

## Versions

Releases are tagged `vX.Y.Z` (see [CHANGELOG.md](CHANGELOG.md)) and publish:

* `ghcr.io/farzai/github-runner-railway:vX.Y.Z`, `:pg-vX.Y.Z` and `:sudo-vX.Y.Z`
* `ghcr.io/farzai/github-runner-railway-controller:vX.Y.Z`

Commits on `main` also publish images under the commit sha, but only for the images they change (docs/deploy.md says which sha is current). There is no `latest`:
pin a version (or a digest, `image@sha256:...`) so a runner never changes under you.

## Troubleshooting

| Symptom | Look at |
| --- | --- |
| The controller exits at start | its log names the missing or malformed variable (never its value) |
| `/healthz` 503 | no cycle has completed yet, the last one is older than three polls, or cycles keep failing (`consecutive_failures`, `unreadable_scopes`); see the `cycle failed` log lines |
| `/healthz` `"error": true` | the last cycle failed; the `cycle failed` log line says why (often a GitHub or Railway API error, which the next cycle retries) |
| A job stays queued | the job's labels must all be among `self-hosted`, `linux`, `x64` and your label; its repository must be in `RUNNER_SCOPES` and the App installed on it; `MAX_RUNNERS` may be reached |
| How often cycles run, and what they cost | every cycle logs one `cycle` line: `duration`, `github_calls`, `railway_calls`, `created`, `reaped`, `failed` |
| Runner services pile up | each should go about a minute after its job ends ("runner gone" in the log); check the Railway token can delete services |
| `docker: command not found` | expected: see Limits |

## Security

See [SECURITY.md](SECURITY.md).

## Develop

```
go vet ./... && go test -race ./...
docker build --platform linux/amd64 -t runner image/
docker build --platform linux/amd64 --target pg -t runner-pg image/
```

Contributions: [CONTRIBUTING.md](CONTRIBUTING.md). License: [LICENSE](LICENSE).
