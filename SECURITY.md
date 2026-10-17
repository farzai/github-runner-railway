# Security

This fleet is meant for **private repositories** of one owner or team. Every job is still treated as
hostile: a dependency, a compromised account or a fork pull request can run code on a runner.

## What a job can see

* The runner container holds **only its own JIT config** (a one-job registration, useless after the
  job). It has no GitHub App key, no webhook secret and no Railway token: those live in the
  controller, which runs in a separate Railway project so a job cannot reach it over the private
  network.
* The image removes `sudo` and the `docker` group from the runner user and drops Railway's own
  variables before the runner starts.
* One container per job: nothing a job leaves behind (processes, files, caches) can reach the next job.

## Rules for the operator

* **Private repositories only.** Runner services share a Railway private network, so a job from a
  public repository (anyone can open a pull request) could reach something a concurrent job started.
  Keep public repositories on GitHub-hosted runners. `RUNNER_SCOPES` decides what is served: list
  private repositories one by one, and use an `org:` scope only when every repository the App can see
  there is private.
* In each organization and repository that uses these runners: Settings, Actions, General, "Approval
  for running fork pull request workflows": **Require approval for all outside collaborators**.
* Do not use `pull_request_target` or `workflow_run` with a checkout of untrusted code on these runners,
  and keep deployment secrets out of workflows that run on `pull_request` from forks.
* For an `org:` scope, use a dedicated runner group limited to selected repositories with "Allow public
  repositories" off.
* Install the GitHub App only where it is needed: Administration write is broad. Two Apps (for example
  one per organization) limit what one leaked key reaches.
* Keep nothing else in the runner Railway project, and run the controller in a different project.
* Set a Railway usage limit on the runner project, so a runaway queue cannot run up a bill.
* Pin the images to a version or digest.

## Two pools in one runner project

Two controllers (for example the plain and the Postgres image) can share the runner project: each lists
and removes only services under its own `RUNNER_SERVICE_PREFIX`, and only takes jobs that ask for its
label. Their runners share the private network, so both pools must serve the same trust level
(private repositories). The Postgres helper `ci-postgres` listens on 127.0.0.1 only.

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
