# Deploying the fleet

Public and private repositories never share a runner environment. Services in one Railway environment
share a private network, so a job from a public repository (anyone can open a pull request) could reach
something a concurrent private job started (SECURITY.md). So there are **two runner pools**, each with its
own Railway project (or environment), its own label, its own GitHub runner group and its own controller
service; the controllers themselves live in a third project so no job can reach them.

| | Public pool | Private pool |
| --- | --- | --- |
| Railway project | **Runner Public** (`d40ef972-9690-4ee0-ab42-a0ec7a8743e1`, production `bd845950-92d0-416a-b227-dde1106c8664`) | **Runner Private** (to be created; its own project token) |
| Label (`RUNNER_LABEL`) | `railway-public` | `railway-private` |
| Repositories (`RUNNER_SCOPES`) | public repositories only | private repositories only, for example `repo:parsilver/skyabove` (private) and the private farzai repositories, listed one by one |
| GitHub runner group | a dedicated group, selected repositories, "Allow public repositories" **on** | a dedicated group, selected private repositories, public repositories **off** |
| Workflows use | `runs-on: [self-hosted, railway-public]` | `runs-on: [self-hosted, railway-private]` |
| First user | later (public farzai repositories) | the pilot, `parsilver/skyabove` |

With one pool only, the README's default label `railway-farzai` is fine; the labels above are for running
both. An org scope (`org:farzai`) is only ever safe in the private pool when every repository the App can
see under it is private; prefer listing repositories.

| Project | Holds | Token |
| --- | --- | --- |
| **Runner Controller** (`daf3516f-92d3-452f-a151-d852ffbeb228`, production `c62b5a0c-d534-4b43-b21d-b0e53f1179b5`) | one controller service per pool | its own project token, used to deploy the controllers |
| the pool's runner project | nothing but the one-job runner services its controller creates and deletes | that project's token, set on its controller as `RUNNER_RAILWAY_TOKEN` |

## Decision: one App, two pools, and what runs first

The GitHub App `farzai-railway-runner` (App ID 5355265) is installed on farzai and parsilver for all
repositories and has one webhook URL, `https://controller-production-0620.up.railway.app/webhook`. One URL
means one process receives every event, so the split between the public and the private pool cannot be
two controllers behind two webhooks: it has to be **inside one controller process, by label** (each pool
its own label, runner project, runner group and scopes, the controller starting one reconcile loop per
pool and routing each event to the pool whose label the job asked for). That multi-pool controller is not
built yet. Until it is:

- **Phase 1 (now): the pilot runs a single pool.** `parsilver/skyabove` is private and only private
  repositories use the pool, in the `railway` runner group (id 3, "all repositories", public repositories
  allowed). No public repository is switched while the pool is shared, because a job from a public
  repository would share a Railway environment and private network with a private job
  (SECURITY.md).
- **Phase 2 (before any public repository moves): build the multi-pool controller**, create a **Runner
  Private** project and a private-only runner group, and move the private repositories to it; the shared
  `Runner Public` project and group `railway` then serve public repositories only.

## Before a controller can start (owner)

1. **One GitHub App per pool** (permissions in the README; installed on the farzai org and that pool's
   repositories; subscribe to **Workflow job**; webhook URL `https://<that controller's domain>/webhook`; its own
   webhook secret). A GitHub App has one webhook URL and the controller's path is fixed, so with one shared App
   only one controller would receive events and the other would run on polling alone (every 120 s). One App per
   pool also limits what a leaked App key can reach (Administration write is broad).
2. **Two dedicated runner groups** in farzai, as in the table (public: public repositories allowed; private:
   private repositories only); each id is that pool's `RUNNER_GROUP_ID`. A personal repository
   (`parsilver/...`) has no groups: its runner is repository-scoped.
3. In farzai and each parsilver repository: Settings, Actions, General, require approval for fork pull
   request workflows from all outside collaborators.
4. Make the GHCR packages public (`github-runner-railway` for the runner, `github-runner-railway-controller`
   for the controller), or give Railway registry credentials.
5. Create the **Runner Private** Railway project and give its project token to the private controller.

## A controller service (Runner Controller project, one per pool)

Image: `ghcr.io/farzai/github-runner-railway-controller:<commit sha>` (CI publishes sha tags only; it is a
static binary on distroless, running as a non-root user, about 9 MB). Port 8080, a public domain for the
webhook, healthcheck path `/healthz` (503 until a cycle has completed).

Variables (never put them in the repository or a log):

| Variable | Value |
| --- | --- |
| `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET` | from the GitHub App |
| `RUNNER_RAILWAY_TOKEN`, `RUNNER_RAILWAY_PROJECT_ID`, `RUNNER_RAILWAY_ENVIRONMENT_ID` | the pool's runner project (Runner Public for the public pool, Runner Private for the private one) |
| `RUNNER_LABEL` | `railway-public` or `railway-private` |
| `RUNNER_IMAGE` | `ghcr.io/farzai/github-runner-railway:<commit sha>` |
| `RUNNER_SCOPES` | that pool's repositories only, e.g. `repo:parsilver/skyabove` for the private pool |
| `RUNNER_GROUP_ID` | the dedicated group's id (required for an org scope) |
| `MAX_RUNNERS` | start small (for example 4) |

Do not deploy it before the App exists: without `GITHUB_APP_ID` it exits at start with a config error
(that is the intended behaviour, and the failure names only the variable).

## Rollout and rollback

1. Deploy the **private-pool** controller (the pilot, `parsilver/skyabove`, is private) with `MAX_RUNNERS=1`
   and `RUNNER_SCOPES=repo:parsilver/skyabove`.
2. Open a pull request in the pilot repository that changes `runs-on` to `[self-hosted, railway-private]`;
   watch `/healthz`, the controller log and the runner service appear and disappear.
3. Raise `MAX_RUNNERS` and add scopes one at a time. Show a scale-up to N and back to none with no killed job.
4. **Rollback:** flip the pilot's `runs-on` back to `ubuntu-latest`, stop the controller (scale the service
   to zero or delete it; `MAX_RUNNERS` cannot be 0), and delete any `rr-*` services left in
   the pool's runner project (the controller's reaper removes strays past the job timeout anyway).

## Rate limits and costs

Railway allows 1,000 API requests an hour per token: a job costs four calls and a cycle one, about 230
jobs an hour. Set a Railway usage limit on both runner projects (Runner Public and Runner Private) and on Runner Controller. The cold start of the runner image was measured at
21.6 s (docs/spike.md).
