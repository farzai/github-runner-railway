# Deploying

A step-by-step guide with placeholders. Replace every `<...>` with your own value; nothing here is
specific to one deployment. The README's quick start is the short version.

## The shape

| Railway project | Holds | Token |
| --- | --- | --- |
| **Controller project** | one controller service per pool | used only to deploy the controller |
| **Runner project** | nothing but the one-job runner services the controllers create and delete | a project token, given to the controller as `RUNNER_RAILWAY_TOKEN` |

Keep them apart: services in one Railway environment share a private network, and a job must not
reach the controller (it holds the App key and the Railway token). Keep nothing else in the runner
project.

Serve **private repositories only** (see SECURITY.md). A pool is one controller with one label and one
runner image; most setups need one pool, and the optional Postgres pool is a second controller.

## Before the controller can start

1. **Railway.** Create the two projects. In the runner project, create a project token and note the
   project id and the environment id (Project Settings).
2. **GitHub App.** Create it from [`github-app-manifest.json`](github-app-manifest.json) or by hand
   (permissions and event in the README). Set the webhook URL once the controller has a domain
   (step 4), generate a webhook secret and a private key, and install the App on the repositories you
   serve (or the organization).
3. **Repository and organization settings.**
   * Settings, Actions, General: require approval for fork pull request workflows from all outside
     collaborators, in every repository and organization the App is installed on.
   * For an `org:` scope only: a dedicated runner group (Settings, Actions, Runner groups) limited to
     the repositories you serve, "Allow public repositories" off. Its id is `RUNNER_GROUP_ID`. A
     `repo:` scope makes repository-scoped runners and ignores the group.
4. **Images.** The published images are public on GHCR. If you build your own, make the package
   public or give Railway registry credentials.

## The controller service

In the controller project, a service from `ghcr.io/farzai/github-runner-railway-controller:<version>`
(a static binary on distroless, non-root, about 9 MB). Port `8080`, a public domain for the webhook,
healthcheck path `/healthz`, restart on failure.

| Variable | Value |
| --- | --- |
| `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET` | from the App |
| `RUNNER_RAILWAY_TOKEN`, `RUNNER_RAILWAY_PROJECT_ID`, `RUNNER_RAILWAY_ENVIRONMENT_ID` | the runner project |
| `RUNNER_IMAGE` | `ghcr.io/farzai/github-runner-railway:<version>` |
| `RUNNER_SCOPES` | `repo:<owner>/<repo>`, comma-separated; `org:<org>` only when every repository the App sees there is private |
| `RUNNER_LABEL` | your label, e.g. `railway` |
| `RUNNER_GROUP_ID` | the runner group id, for an `org:` scope |
| `MAX_RUNNERS` | start with `1` |
| `MAX_JOB_MINUTES` | at least your longest job's timeout |

Never put these values in a repository or a log. The controller exits at start when one is missing,
naming the variable only. Then set the App's webhook URL to `https://<controller domain>/webhook`.

`RUNNER_SCOPES` is the real guard of what the pool serves: the controller only makes runners for the
scopes it lists, and a `repo:` scope's runners can only be used by that repository.

## The Postgres pool (optional)

For jobs that need PostgreSQL or Playwright's browsers, a second controller service in the same
controller project:

| Variable | Value |
| --- | --- |
| `RUNNER_LABEL` | e.g. `railway-pg` |
| `RUNNER_SERVICE_PREFIX` | e.g. `rp-` (the first controller keeps `rr-`; each lists and removes only its own services) |
| `RUNNER_IMAGE` | `ghcr.io/farzai/github-runner-railway:pg-<version>` |
| `RUNNER_SCOPES` | only the repositories that need it |
| `POLL_SECONDS` | `60`: the App's one webhook URL goes to the first controller, so this one polls |
| everything else | as the first controller (same App, same runner project and token) |

A job there runs `ci-postgres start <user> <password> <database>` and reads `CI_POSTGRES_URL`
(README). The Playwright browsers of the version pinned in `image/Dockerfile` are baked in under
`PLAYWRIGHT_BROWSERS_PATH=/ms-playwright`, so `playwright install` of that version downloads nothing;
a job on another Playwright version downloads its own browsers into the same directory.

## Rollout and rollback

1. Deploy with `MAX_RUNNERS=1` and one private pilot repository in `RUNNER_SCOPES`.
2. In the pilot repository, open a pull request that sets `runs-on: [self-hosted, <label>]` on one job
   (and declares the label for actionlint). Watch `/healthz`, the controller log (`created`, then
   `reaping ... runner gone` about a minute after the job), and the runner service appear and go.
3. Make every job you move Docker-free (no `docker://` actions, `docker` steps, `container:` or
   `services:`) and sudo-free.
4. Add repositories to `RUNNER_SCOPES` one at a time and raise `MAX_RUNNERS` to your parallel jobs.
5. **Rollback:** set the jobs' `runs-on` back to a hosted runner, stop the controller (remove its
   deployment or delete the service; `MAX_RUNNERS` cannot be 0), and delete any `<prefix>*` services
   left in the runner project.

## Upgrades

Pin both images to a release (`:vX.Y.Z`, `:pg-vX.Y.Z`) or a digest. To upgrade, change
`RUNNER_IMAGE` and the controller's image to the new version and redeploy the controller; runners
already running finish on the old image. Read CHANGELOG.md for anything that needs a new variable.

## Which image tag is current

A push to `main` rebuilds the runner images only when `image/` changed, and the controller image only
when the Go code or its Dockerfile changed, so not every commit has every sha tag. The newest runner
image is the sha of the last commit that touched `image/` (`git log -1 --format=%H -- image/`), and
the newest controller image the last commit that touched `cmd/`, `internal/`, `go.mod` or
`Dockerfile`. A commit that changes only `ci.yml` rebuilds and tags all three. If an image build failed or was
cancelled and the next push did not touch that image's sources, that sha has no image: check the tag
exists (`docker manifest inspect ghcr.io/<owner>/<repo>:<sha>`) and re-run the failed workflow run if
not. Releases (`vX.Y.Z` tags) always publish all three; prefer those to sha tags.

## Rate limits and costs

See "Sizing" below. Railway's usage limits are set per **workspace**, not per project, and reaching the hard limit takes
*every* workload in the workspace offline (Railway docs, usage limits, read 2026-10-17). Set a soft
limit (an email alert) for the runner fleet, and a hard limit only if the workspace holds nothing
else you need running.

## Sharing the runner project with long-lived services

Runners can run in a project next to services that stay up, such as a shared CI database or a cache,
and reach them over the private network (`<service>.railway.internal`). The controller lists, counts
and deletes only services whose names are exactly `<RUNNER_SERVICE_PREFIX><6 hex>-<6 hex>`, the names
it makes; any other service, including one that starts with the prefix (`rr-postgres`), is never
touched (tested in `internal/railway`). Still:

* Pick a prefix no other service in the project uses, and give each controller its own.
* Everything a job runs can reach those services, and any concurrent job can too: use this only for
  private repositories of one trust level, and give the shared services their own credentials.
* A shared database is state that outlives jobs: tests must clean up after themselves or use their
  own database or schema per job. The `pg` and `redis` images give each job its own server instead,
  with nothing shared, at the cost of a few seconds of start-up per job.
* The controller itself must still run in a different project (it holds the App key and the token).

## Sizing

* **`MAX_RUNNERS`**: the most jobs you want running at once: for one repository, its widest matrix
  times the workflows that can run together; for several, the sum at your busiest hour. Each runner
  is a container with its own CPU and memory, so this is also your spend cap. Watch `queued` in
  `/healthz` and the `cycle` log line: jobs that wait while `services` equals `MAX_RUNNERS` mean the
  cap is too low.
* **`MAX_CREATE_PER_CYCLE`**: how many runners one cycle may start, about the size of a burst (one
  push's matrix). Higher starts a burst faster but spends more Railway calls at once.
* **Railway API**: per token, by plan: 100 requests an hour (Free), 1,000 (Hobby), 10,000 (Pro), plus
  10 (Hobby) or 50 (Pro) a second (Railway docs, public API, read 2026-10-17). The limit actually
  applied is in the `X-RateLimit-Limit` header of every answer; a Pro workspace was observed at 1,000 an
  hour for project tokens on 2026-10-17, so plan for the header, not the table. A job costs about four
  calls (create, configure, deploy, delete) and every cycle one list call; controllers that share a
  token share its budget. At 1,000 an hour and the default poll that is roughly 230 jobs an hour.
* **GitHub API**: 5,000 requests an hour per installation (more above 20 repositories), shared by
  every scope of that installation. A cycle reads, per repository, two run lists, the jobs of each
  active run and the runner list; unchanged lists come back as `304` and do not count. `/healthz`
  shows `github_rate_limits`, and a warning is logged below a tenth.
* **Railway per-service limits** (Railway docs, plans, read 2026-10-17): up to 48 vCPU and 48 GB per
  service on Hobby, 1,000 vCPU and 1 TB on Pro, 100 GB ephemeral storage, image size 100 GB on Hobby.
  A runner service gets the workspace's default per-service limit (observed: 24 vCPU, 24 GB and a
  limit of about 1,000 processes and threads, 2026-10-17). Railway documents no limit on services per
  project; a fleet of a few dozen short-lived services at once has worked.
