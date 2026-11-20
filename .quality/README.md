# Quality gate

The `quality` job in `.github/workflows/ci.yml` runs on every pull request and calls `gate.py`. It follows the
engineering quality standard, which works as a ratchet: only code the PR adds or changes is judged, so debt already
on main never blocks a PR.

| Check | Fails when | Tool |
|---|---|---|
| Format | `gofmt -l` lists a Go file the PR adds or changes, tests included | gofmt |
| Coverage | an executable line the PR adds or changes is never run by a test. The repo total is reported, never a failure | `go test -coverprofile` |
| CRAP | a function the PR adds or changes scores 10 or more, unless it was already 10 or more on base and scores no higher now | crapper |
| Duplication | the PR adds a structural duplicate pair at 0.8 similarity or above | dryer |
| Mutation | a mutant on a changed line survives or gets no verdict, mutator fails, the tests fail before any mutation, or the budget sized to the change runs out | mutator |

Scope is Go under `cmd/` and `internal/`, minus `*_test.go` files and `testdata/` directories. A pure deletion
judges nothing.

Changed files (added, modified, or changed in type) are listed with `git diff --numstat -z`, so every name arrives
whole, and each file's lines come from its own `-U0` patch. The gate stops with a message, before any check, when a
changed name under `cmd/` or `internal/` is not a plain repository path (only letters, digits, `_`, `-`, `.` and `/`,
no `..` part), when a Go file there is binary, a symbolic link or a submodule, or when a file's hunks do not add up to
the lines git counts for it: a file the gate cannot read must never pass as unchanged.

## Coverage

`go test -coverprofile` writes module paths (`github.com/farzai/github-runner-railway/internal/...`). mutator, at the
pinned commit, matches a profile entry only when the checkout's path ends with the module path, so on CI it would mark
every site as uncovered and mutate nothing. The job writes the raw profile to `target/coverage/profile.out`, and the
gate writes a copy with repository paths to `target/coverage/coverage.out`, which both crapper and mutator read.

A line is executable when a block of the profile spans it. It is untested when every block that spans it has a count of
0, unless the line is blank, a comment, or only braces and parentheses. A changed file the profile does not list at all
passes only when it holds no function body with a statement in it (types, constants, empty functions); otherwise
coverage fails for the whole file, and no waiver covers it. A file behind a build tag that no test enables is the usual
cause.

The profile is built without `-coverpkg`, the same way mutants are tested (only the mutated file's package), so a line
counts as covered only when its own package's tests run it.

The repo total is statement coverage over the scope, as `go tool cover -func` totals it. Following the standard's
"Coverage on deletions" section, a drop in the total does not fail a PR on its own, because deleting covered code can
cause it. The total is printed next to `baseline.json`, and the gate asks for an update when it improves.

## Overrides

Kill a surviving mutant, or cover an untested line, with a test. When no test can (an equivalent mutant, or a line
only a real signal or a real network failure reaches, such as `func main`), say so in the PR description, each waiver
a paragraph of its own, with a blank line before and after it:

> Equivalent mutant: internal/reconcile/reconcile.go:42 why the change cannot alter any output

(written in the description as plain text, without the `>`). The reason must be 10 or more characters; reviewers
judge whether it is believable. One waiver covers every mutant, and an untested line, at that file and line number.

The gate reads waivers from GitHub's own rendering of the description (`POST /markdown`, mode `gfm`), so it counts
only what a reviewer sees on the PR page: a waiver counts when it is the whole text of a top-level paragraph. A waiver
on the line right after other text (GitHub joins the two into one paragraph), an indented one, one in a code block,
an HTML comment, a quote, a list or any other element, including a closed `<details>`, waives nothing. Write the reason
as plain text: a mention, a link, inline code or formatting makes the paragraph more than text. Nor does a paragraph
with a bidirectional control character anywhere in it. The `Waivers` section of the report lists the ones that counted.

The job's first step fetches the description and has GitHub render it; it is the only step that holds the token, and
it runs before any code from the pull request. The rendered file's sha256 goes to the gate as a step output, and the
gate fails when the file no longer matches it. After editing the description, re-run the `quality gate` job.

The hash protects the rendered description only. A pull request can change `gate.py`, `mutant-tests.sh` or the
workflow, and the job runs that version, so review of `.quality/` and `.github/` remains the control.

## CRAP ratchet

The owner's rule (2026-11-05): a changed function that already scores 10 or more on base passes when its CRAP is no
higher than on base; the report lists it as "still over the limit" with both scores. A new function, a moved or
renamed one, and one that was under 10 on base must stay under 10. Functions match by file, receiver and name, and
order among same-named functions in the file, so a move or rename counts as new; when the count of a name in a file
changes, every function of that name counts as new. A base function with no coverage record is not scored, so it is
judged as new.

The base score is measured only when a touched function is over 10: the gate checks out the base in a throwaway
worktree and runs the tests with coverage there, within 240 s. When that does not finish or fails, CRAP fails with a
message to re-run the job once.

## Duplication

dryer runs on the base and on the PR. A pair on the PR that touches a changed line is new unless a base pair between
the same two files still holds both of its sides: the base lines are followed through each file's patch, the edits
inside a side stay within the old side, and the two sides sit at overlapping places. So removing one pair and adding
another between the same two files fails, as does growing a side past its old end; editing inside an old pair, or
moving it, does not.

## Mutation

Mutation runs last, and only when every other check passed (the report says so when it was skipped). The gate gives
mutator each changed file with its changed lines (`--lines`) rather than using `mutator --changed`, which reads
`git status` and finds nothing on a clean CI checkout. `--mutate-all` keeps an old `.metrics/` snapshot from carrying
killed results over.

mutator runs each mutant in a copy of the tree under `target/mutation-workers`. `mutant-tests.sh` runs
`go test -count=1` on the mutated file's own package there, and logs the verdict. Go's build cache is safe to share,
so mutants of one file run in parallel, one per CPU of the runner (from the cgroup's `cpu.max` when it sets a quota).

The gate first scans each changed file for mutation sites, then requires a KILLED or SURVIVED verdict for every site
it found. It fails rather than passes when the scan exits non-zero or prints anything but one `Scan: N mutation
sites` header with N site lines under it, and when a site gets no verdict (mutator marks a site no test covers
UNCOVERED and never mutates it). A site with no verdict can be waived as a survivor can. A mutant that does not
compile counts as killed (mutator's rule).

The budget is time, sized to the change. After the scan, the gate times one run of `mutant-tests.sh` on the first
changed file with sites; a failing run fails the check before anything is mutated. The budget is the time spent so
far, plus for each file its sites divided by the workers (rounded up) and one round for mutator's own baseline run,
times that measured time; never below 5 minutes and never above 30. The report prints the budget and its inputs.

When the budget runs out, the gate kills mutator and every process it started (they carry the run's
`QUALITY_GATE_TAG`, found through `/proc`, so Linux only), fails, and lists what was killed, what survived and which
files were not finished. Below the cap, re-run the job once (the runner may have been busy). A change that cannot
finish within 30 minutes is split into smaller PRs, or its tests are made faster.

## Ratchet

`baseline.json` holds the numbers measured on main. They may only get better: a PR that improves one updates the
file, and the gate says so for coverage and duplicate pairs. The gate only reports this; review, not the gate, keeps
`baseline.json` from getting worse. Never lower a number, a threshold or a check to get a PR through; that is an owner
decision.

## Run it locally

Needs Python 3.12 or later and Go.

```sh
python3 -m venv /tmp/qvenv && /tmp/qvenv/bin/pip install -r .quality/requirements.txt
mkdir -p target/coverage && go test -count=1 -coverprofile=target/coverage/profile.out ./...
PATH=/tmp/qvenv/bin:$PATH python .quality/gate.py --base origin/main   # no waivers count without --description-html
PATH=/tmp/qvenv/bin:$PATH python -m unittest discover -s .quality -p 'test_*.py'   # the gate's own tests
```

Stopping mutants at the budget needs `/proc`, so on macOS a run that hits the budget can leave `go test` processes
behind.
