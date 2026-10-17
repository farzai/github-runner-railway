# Railway spike (2026-10-17)

Measured against a runner project with a project token, one throwaway `alpine:3.20` service.

| Question | Result |
| --- | --- |
| Can a project token create a service from an image? | Yes (`serviceCreate`, 0.4 s) |
| Does creating it deploy it? | No. `serviceInstanceDeployV2` starts the first deploy, so the config (restart policy NEVER) is set before any container runs |
| Cold start of a tiny image | INITIALIZING 1.4 s, DEPLOYING 4.7 s, SUCCESS 8.1 s after the deploy call |
| Can a project token delete a service? | Only with the environment: `serviceDelete(id, environmentId)`. Without `environmentId` it is "Not Authorized" |
| API rate limit | `RateLimit-Policy: "default";q=1000;w=3600` - 1,000 requests an hour per token |

Not yet measured: cold start with the real runner image (about 1.3 GB), the project's service-count
limit, and what a container that exits leaves as its deployment status (the controller treats FAILED,
CRASHED, REMOVED, REMOVING, COMPLETED and SKIPPED as ended and also reaps any service that never
registered a runner within the startup grace).

## Cold start of the real runner image (2026-10-17)

A service from `ghcr.io/farzai/github-runner-railway:d18b4e82...` (1.3 GB, public package, anonymous pull) with
restartPolicy NEVER and a `sleep 90` start command: deploy call to a running container **21.6 s**
(INITIALIZING 0.8 s, DEPLOYING 3.1 s, running 21.6 s); deleted with the environment id, project empty
afterwards. One sample; whether Railway caches layers between services is not known.
