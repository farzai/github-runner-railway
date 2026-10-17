# Security

This fleet runs workflows of public repositories too, so every job is treated as hostile.

## What a job can see

* The runner container holds **only its own JIT config** (a one-job registration, useless after the
  job). It has no GitHub App key, no webhook secret and no Railway token: those live in the
  controller, which runs in a separate Railway project so a job cannot reach it over the private
  network.
* The image removes `sudo` and the `docker` group from the runner user and drops Railway's own
  variables before the runner starts.
* One container per job: nothing a job leaves behind (processes, files, caches) can reach the next job.

## Rules for the owner

* In each organization and repository that uses these runners: Settings, Actions, General, "Approval
  for running fork pull request workflows": **Require approval for all outside collaborators**.
  Without it a stranger's pull request runs on our runners.
* Do not put deployment secrets in workflows that run on `pull_request` from forks.
* Use a dedicated runner group limited to selected repositories.
* Install the GitHub App only on the repositories that need it (Administration permission is broad).
* Keep nothing else in the runner Railway project.

## Controller

* Verifies every webhook with HMAC SHA-256 and refuses everything when no secret is set.
* Trusts no event for what it says: it only nudges a poll; creation is driven by what GitHub reports.
* Creates runners only for the scopes in `RUNNER_SCOPES`.
* Never logs the App key, the webhook secret, a JIT config or the Railway token.

## Reporting

Open a private security advisory on this repository.
