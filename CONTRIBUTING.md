# Contributing

Thank you for helping. Issues and pull requests are welcome.

## Before you start

* For a bug, open an issue with the controller's log lines (they never contain secrets; check before
  you paste anything else), your versions, and what you expected.
* For a feature, open an issue first so we can agree on the shape; the controller is kept small on
  purpose.
* Security problems: do not open an issue; see [SECURITY.md](SECURITY.md).

## Working on the code

```
go vet ./... && go test -race ./...
docker build --platform linux/amd64 -t runner image/
docker build --platform linux/amd64 --target pg -t runner-pg image/
```

* Go standard library only in the controller, unless there is a strong reason.
* Write the test first and see it fail; every behaviour change needs a test.
* Use example values in tests and docs (`acme`, `repo:acme/app`), never a real organization,
  repository, project id or token.
* Pin everything the images download (a version and a checksum or digest).
* Keep secrets out of errors and logs: errors name a variable, never its value.

## Pull requests

* One change per pull request, with a [Conventional Commits](https://www.conventionalcommits.org)
  title (`feat: ...`, `fix(reconcile): ...`, `docs: ...`).
* Update README.md, docs/deploy.md and the `[Unreleased]` section of CHANGELOG.md when behaviour or
  configuration changes.
* CI runs `go vet`, the tests with the race detector, and builds the three images.

## Releases

Maintainers tag `vX.Y.Z` on `main` after moving `[Unreleased]` in CHANGELOG.md to the new version; CI
publishes the versioned images from the tag.
