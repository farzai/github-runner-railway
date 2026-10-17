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
* **Separate pools for public and private repositories.** Services in one Railway environment share a
  private network, so a hostile job from a public repository could reach something a concurrent
  trusted job started. Run two controllers: one with `RUNNER_SCOPES` of public repositories and its own
  `RUNNER_LABEL` (for example `railway-public`) in its own Railway project or environment, and one for
  private repositories. Never mix them in one environment.
* Do not use `pull_request_target` or `workflow_run` with a checkout of untrusted code on these runners.
* Set a Railway usage limit on the runner project, so a runaway queue cannot run up a bill.
* Prefer two GitHub Apps (one for the org, one for personal repositories): Administration write is
  broad, and a leaked key then reaches only one side.

## The JIT config

The runner service holds its JIT config as an environment variable and the runner reads it at start,
so a job on that runner can read it (through its own environment or `/proc`). It registers only that
one runner, which is deleted when the job ends; the impact is limited to the job that already runs
on it. The image also unsets Railway's own variables before the runner starts.

## Controller

* Verifies every webhook with HMAC SHA-256 and refuses everything when no secret is set.
* Trusts no event for what it says: it only nudges a poll; creation is driven by what GitHub reports.
* Creates runners only for the scopes in `RUNNER_SCOPES`, and requires a dedicated runner group (never the Default group) for an org scope.
* Never follows an HTTP redirect (the App token and the Railway token must go only where they were pointed) or a pagination link to another host or scheme.
* Never logs the App key, the webhook secret, a JIT config or the Railway token.

## Reporting

Open a private security advisory on this repository.
