# github-runner-railway

Autoscaling self-hosted GitHub Actions runners on [Railway](https://railway.com): **one short-lived
Railway service per job**. A controller watches GitHub for jobs labelled `railway-farzai`, registers
a one-job (JIT) runner for each, starts it as its own Railway service, and deletes the service when
the job is over. Nothing is shared between two jobs.

```
GitHub App webhook (workflow_job)  -->  controller  -->  Railway API
                                          |   ^             |
                 poll queued jobs/runners |   |             v
                                          v   |      runner service (one per job)
                                        GitHub          run.sh --jitconfig  ->  one job  ->  exit
```

## Why one service per job

* **Isolation.** A runner container that outlives a job can be left with background processes or
  edited files by a fork PR's job and hand them to the next job. A fresh container per job cannot.
* **Safe scale-down.** Railway cannot choose which replica of a service to stop, so scaling a pool
  down can kill a busy runner. Here the controller deletes exactly the service it picked, and only
  after GitHub agreed the runner is idle (GitHub refuses to delete a busy runner).
* **No restart burn.** Services are created with `restartPolicyType = NEVER`.
* **Personal accounts.** A personal account has no org runners or runner groups; its repositories
  need repo-scoped JIT configs, which only a per-job creation can serve.

## Parts

| Path | What |
| --- | --- |
| `cmd/controller` | the controller binary (HTTP: `/webhook`, `/healthz`) |
| `internal/reconcile` | the pure planner: what to create and what to remove |
| `internal/ghapp` | GitHub App auth, queued jobs, runners, JIT configs (standard library only) |
| `internal/railway` | create, list and delete runner services |
| `internal/webhook` | HMAC-verified `workflow_job` receiver; only nudges, the poll is the truth |
| `image/` | the runner image |

The webhook is only a nudge. GitHub does not redeliver a missed event, so the controller also polls
GitHub and Railway every `POLL_SECONDS` and reconciles what it sees: it creates a runner service for
every queued job no runner is coming for, and removes dead, idle-surplus or overdue services.

## GitHub App

Permissions: repository **Administration: Read and write**, **Actions: Read**, **Metadata: Read**;
organization **Self-hosted runners: Read and write**. Subscribe to **Workflow job**. Install it on the
org and on the personal repositories you serve (selected repositories only, since Administration is
broad). Under the org: Settings, Actions, Runner groups: use a dedicated group limited to selected
repositories, and turn on "Allow public repositories" there only if public repositories use it.

## Configuration (controller variables)

| Variable | Meaning | Default |
| --- | --- | --- |
| `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET` | the App (PEM may keep `\n` escaped) | required |
| `RAILWAY_API_TOKEN`, `RAILWAY_PROJECT_ID`, `RAILWAY_ENVIRONMENT_ID` | a project token for the **runner** project | required |
| `RUNNER_IMAGE` | image the runner services use, e.g. `ghcr.io/farzai/github-runner-railway:latest` | required |
| `RUNNER_SCOPES` | comma list of `org:<name>` and `repo:<owner>/<name>` that may get runners | required |
| `RUNNER_LABEL` | the `runs-on` label | `railway-farzai` |
| `RUNNER_GROUP_ID` | runner group of org runners | `1` |
| `MAX_RUNNERS` / `MAX_CREATE_PER_CYCLE` | cap of services in all / created in a cycle | `8` / `4` |
| `POLL_SECONDS` | polling period | `120` |
| `STARTUP_GRACE_SECONDS` / `IDLE_TTL_SECONDS` | time to register / keep a surplus idle runner | `300` / `120` |
| `MAX_JOB_MINUTES` | job timeout; a service older than this plus 10 minutes is removed | `360` |
| `ALERT_QUEUED_MINUTES` / `ALERT_WEBHOOK_URL` | alert when a job waits this long with no runner coming | `5` / none |
| `PORT` | HTTP port | `8080` |

Use it in a workflow with `runs-on: [self-hosted, railway-farzai]`.

## Limits worth knowing

* **Railway API:** 1,000 requests an hour per token. A job costs four calls (create, configure,
  deploy, delete) and each cycle one list call, so about 230 jobs an hour at the default poll.
* **GitHub API:** an installation has 5,000 requests an hour; each cycle reads, per repository of an
  org scope, the queued and in-progress runs and their jobs. Prefer `repo:` scopes for large orgs.
* **No docker** in the runner (a Railway container has no daemon): jobs that need `docker build`,
  service containers or `container:` cannot run here. Use `ubuntu-latest` for those.
* **No sudo** in the image, so `sudo apt-get install` fails; use `setup-*` actions or tools in the image.
* **pids and cores:** each container caps processes at about 1,000 and `nproc` shows the host's cores,
  not the container's: pass an explicit `-j` to compilers.
* **Cold start** is the Railway deploy plus the image pull; see `docs/spike.md` for measurements.

## Security model

See [SECURITY.md](SECURITY.md). In short: ephemeral JIT runners that take one job; the runner service
holds only its own JIT config; the controller (which holds the App key and the Railway token) runs in
a **different Railway project** from the runners; fork-PR workflows must require approval.

## Develop

```
go vet ./... && go test -race ./...
docker build -t runner image/
```
