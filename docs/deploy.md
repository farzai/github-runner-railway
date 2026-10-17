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

Railway allows 1,000 API requests an hour per token: a job costs about four calls and a cycle one.
Set a Railway usage limit on the runner project and on the controller project.
