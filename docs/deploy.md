# Deploying the fleet

Two Railway projects, on purpose: services in one project share a private network, so the controller
(which holds the GitHub App key and a Railway token) must not sit next to the runners that run
untrusted jobs.

| Project | Holds | Token |
| --- | --- | --- |
| **Runner Controller** (`daf3516f-92d3-452f-a151-d852ffbeb228`, env production `c62b5a0c-d534-4b43-b21d-b0e53f1179b5`) | the controller service only | its own project token, used to deploy the controller |
| **Runner Public** (`d40ef972-9690-4ee0-ab42-a0ec7a8743e1`, env production `bd845950-92d0-416a-b227-dde1106c8664`) | nothing but the one-job runner services the controller creates and deletes | the project token the controller uses as `RAILWAY_API_TOKEN` |

For public and private repositories run two controllers (see SECURITY.md), each with its own
`RUNNER_LABEL`, `RUNNER_SCOPES` and runner project or environment.

## Before the controller can start (owner)

1. **GitHub App** with the permissions in the README, installed on the farzai org and the selected
   parsilver repositories; subscribe to **Workflow job**; webhook URL `https://<controller domain>/webhook`
   and a webhook secret.
2. **A dedicated runner group** in farzai (repositories selected, public repositories allowed only if public
   repositories use it); its numeric id is `RUNNER_GROUP_ID`.
3. In farzai and each parsilver repository: Settings, Actions, General, require approval for fork pull
   request workflows from all outside collaborators.
4. Make the GHCR packages public (`github-runner-railway` for the runner, `github-runner-railway-controller`
   for the controller), or give Railway registry credentials.

## The controller service (Runner Controller project)

Image: `ghcr.io/farzai/github-runner-railway-controller:<commit sha>` (CI publishes sha tags only; it is a
static binary on distroless, running as a non-root user, about 9 MB). Port 8080, a public domain for the
webhook, healthcheck path `/healthz` (503 until a cycle has completed).

Variables (never put them in the repository or a log):

| Variable | Value |
| --- | --- |
| `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET` | from the GitHub App |
| `RAILWAY_API_TOKEN` | the project token of **Runner Public** |
| `RAILWAY_PROJECT_ID` | `d40ef972-9690-4ee0-ab42-a0ec7a8743e1` |
| `RAILWAY_ENVIRONMENT_ID` | `bd845950-92d0-416a-b227-dde1106c8664` |
| `RUNNER_IMAGE` | `ghcr.io/farzai/github-runner-railway:<commit sha>` |
| `RUNNER_SCOPES` | e.g. `org:farzai,repo:parsilver/skyabove` |
| `RUNNER_GROUP_ID` | the dedicated group's id (required for an org scope) |
| `MAX_RUNNERS` | start small (for example 4) |

Do not deploy it before the App exists: without `GITHUB_APP_ID` it exits at start with a config error
(that is the intended behaviour, and the failure names only the variable).

## Rollout and rollback

1. Deploy the controller with `MAX_RUNNERS=1` and `RUNNER_SCOPES` of the pilot repository only.
2. Open a pull request in the pilot repository that changes `runs-on` to `[self-hosted, railway-farzai]`;
   watch `/healthz`, the controller log and the runner service appear and disappear.
3. Raise `MAX_RUNNERS` and add scopes one at a time. Show a scale-up to N and back to none with no killed job.
4. **Rollback:** flip the pilot's `runs-on` back to `ubuntu-latest`, stop the controller (scale the service
   to zero or delete it; `MAX_RUNNERS` cannot be 0), and delete any `rr-*` services left in
   Runner Public (the controller's reaper removes strays past the job timeout anyway).

## Rate limits and costs

Railway allows 1,000 API requests an hour per token: a job costs four calls and a cycle one, about 230
jobs an hour. Set a Railway usage limit on both projects. The cold start of the runner image was measured at
21.6 s (docs/spike.md).
