# Deploying the fleet

**Private repositories only** (owner decision 2026-10-17). Public repositories stay on GitHub-hosted
runners: a job from a public repository (anyone can open a pull request) could reach a concurrent private
job through the shared Railway private network (SECURITY.md), so there is one pool and it never takes a
public repository. The farzai runner group `railway` (id 3) has "Allow public repositories" **off**, so a
public repository cannot land on a runner of an org scope even by a wrong `runs-on`.

**`RUNNER_SCOPES` is the real guard.** The controller only makes runners for the scopes it lists, and a
`repo:` scope makes repository-scoped runners that only that repository can use, so listing private
repositories one by one and no org scope is what keeps public repositories off the pool. The group
setting is a second layer for org scopes only; it does not apply to a `repo:` scope.

| | Private pool |
| --- | --- |
| Railway project | the runner project (`d40ef972-9690-4ee0-ab42-a0ec7a8743e1`, production `bd845950-92d0-416a-b227-dde1106c8664`, currently named Runner Public) |
| Label (`RUNNER_LABEL`) | `railway-private` |
| Repositories (`RUNNER_SCOPES`) | private repositories, listed one by one, for example `repo:parsilver/skyabove`; no org scope |
| GitHub runner group | `railway` (id 3) for farzai org scopes only; a `repo:` scope makes repository-scoped JIT runners, so `RUNNER_GROUP_ID` is not used for it |
| Workflows use | `runs-on: [self-hosted, railway-private]` |

| Project | Holds | Token |
| --- | --- | --- |
| **Runner Controller** (`daf3516f-92d3-452f-a151-d852ffbeb228`, production `c62b5a0c-d534-4b43-b21d-b0e53f1179b5`) | the controller service | its own project token, used to deploy the controller |
| the runner project | nothing but the one-job runner services the controller creates and deletes | that project's token, set on the controller as `RUNNER_RAILWAY_TOKEN` |

The GitHub App `farzai-railway-runner` (App ID 5355265) has one webhook URL, which feeds the one
controller. It holds Administration write, so before deploying set both installations (farzai and
parsilver) to **selected repositories**, and add each repository as it joins the rollout.

### Rollout order

Jobs use `runs-on: [self-hosted, railway-private]`.

1. `parsilver/skyabove` (pilot).
2. `farzai/polity.farzai.com` phase 1 (Game Lead, issue #154; needs `MAX_JOB_MINUTES` of at least 360;
   Polity's Postgres and WebKit needs are a separate runner image variant).
3. `parsilver/lottery-matching`, `parsilver/geonames-data`, `parsilver/thailand-address-data` (not its
   `fetch` job, which stays on the Mac runner), then `parsilver/me.farzai.com` (CI only).

Each entry goes into `RUNNER_SCOPES` as `repo:<owner>/<name>` (all of the above are private; check that
before adding one). Every repository that moves also needs its actionlint config to know the label
(`.github/actionlint.yaml`: a `self-hosted-runner:` key holding `labels:` with `railway-private`, as in
skyabove) and no Docker at all, because the runners have no Docker daemon: no `docker://` or other
Docker-based action, no `docker` step, no job `container:` and no `services:` (Polity's Postgres needs a
runner image variant instead).

## Before a controller can start (owner)

1. **The GitHub App** (permissions in the README; installed on the selected repositories only; subscribe to
   **Workflow job**; webhook URL `https://<the controller's domain>/webhook`; its own webhook secret).
2. **The runner group** `railway` (id 3) in farzai: public repositories **off**. A personal repository
   (`parsilver/...`) has no groups: its runner is repository-scoped.
3. In farzai and each parsilver repository: Settings, Actions, General, require approval for fork pull
   request workflows from all outside collaborators.
4. Make the GHCR packages public (`github-runner-railway` for the runner, `github-runner-railway-controller`
   for the controller), or give Railway registry credentials.
5. The runner Railway project (`d40ef972-...`, currently named Runner Public) already exists. Give its
   project token to the controller as `RUNNER_RAILWAY_TOKEN`; the controller service itself lives in the
   separate Runner Controller project.

## The controller service (Runner Controller project)

Image: `ghcr.io/farzai/github-runner-railway-controller:<commit sha>` (CI publishes sha tags only; it is a
static binary on distroless, running as a non-root user, about 9 MB). Port 8080, a public domain for the
webhook, healthcheck path `/healthz` (503 until a cycle has completed).

Variables (never put them in the repository or a log):

| Variable | Value |
| --- | --- |
| `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET` | from the GitHub App |
| `RUNNER_RAILWAY_TOKEN`, `RUNNER_RAILWAY_PROJECT_ID`, `RUNNER_RAILWAY_ENVIRONMENT_ID` | the runner project |
| `RUNNER_LABEL` | `railway-private` |
| `RUNNER_IMAGE` | `ghcr.io/farzai/github-runner-railway:<commit sha>` |
| `RUNNER_SCOPES` | private repositories only, listed one by one, e.g. `repo:parsilver/skyabove` |
| `RUNNER_GROUP_ID` | the dedicated group's id (required for an org scope) |
| `MAX_RUNNERS` | start small (for example 4) |

Do not deploy it before the App exists: without `GITHUB_APP_ID` it exits at start with a config error
(that is the intended behaviour, and the failure names only the variable).

## Rollout and rollback

1. Deploy the controller (the pilot, `parsilver/skyabove`, is private) with `MAX_RUNNERS=1`
   and `RUNNER_SCOPES=repo:parsilver/skyabove`.
2. Open a pull request in the pilot repository that changes `runs-on` to `[self-hosted, railway-private]`;
   watch `/healthz`, the controller log and the runner service appear and disappear.
3. Raise `MAX_RUNNERS` and add scopes one at a time. Show a scale-up to N and back to none with no killed job.
4. **Rollback:** flip the pilot's `runs-on` back to `ubuntu-latest`, stop the controller (scale the service
   to zero or delete it; `MAX_RUNNERS` cannot be 0), and delete any `rr-*` services left in
   the pool's runner project (the controller's reaper removes strays past the job timeout anyway).

## Rate limits and costs

Railway allows 1,000 API requests an hour per token: a job costs four calls and a cycle one, about 230
jobs an hour. Set a Railway usage limit on the runner project and on Runner Controller. The cold start of the runner image was measured at
21.6 s (docs/spike.md).
