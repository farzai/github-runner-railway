#!/usr/bin/env python3
"""Pull request quality gate (engineering quality standard, issue #43).

Ratchet, not big bang: only code the pull request adds or changes is judged, so debt that is already on main never
blocks a PR. Five checks, all of them in this one file so a revert removes the gate:

  format     gofmt -l is clean on every Go file the PR adds or changes, tests included
  coverage   every executable line the PR adds or changes must be hit by a test (the go test coverage profile);
             the repo total is reported but never fails a PR, so deleting covered code cannot block a cleanup
  crap       a function the PR adds or changes must score under 10 (crapper), unless it was already 10 or more on
             base and scores no higher now (the base is measured only when that case comes up)
  duplicate  the PR must not add a structural duplicate pair at 0.8 or above (dryer)
  mutation   no surviving mutant on the lines the PR changes (mutator), within a time budget sized to the change
Both coverage and mutation can be waived for one line (never line 0, a whole file missing from the coverage profile)
with a paragraph in the PR description, "Equivalent mutant: <file>:<line> <reason>" (the reason is 10 or more
characters). One such paragraph waives every mutant on that line. It counts only as a top-level paragraph of its own
in GitHub's rendering of the description, which an earlier CI step writes to a file (--description-html) before any
code from the pull request runs.

Scope is Go under cmd/ and internal/, minus *_test.go files and testdata/ directories.

Run from the repository root with crapper, dryer and mutator importable and on the PATH (see
.quality/requirements.txt), after `go test -coverprofile=target/coverage/profile.out ./...`. go test writes module
paths into the profile, which mutator cannot match to a checkout, so the gate first writes a copy with repository
paths to target/coverage/coverage.out, where crapper and mutator read it.

Exit 0 when every check passes, 1 when any fails. The report goes to stdout and to GITHUB_STEP_SUMMARY.
"""

from __future__ import annotations

import argparse
import functools
import hashlib
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid
from collections import Counter
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from html.parser import HTMLParser
from pathlib import Path
from typing import NamedTuple

# Never leave __pycache__ in the tree.
sys.dont_write_bytecode = True

CRAP_LIMIT = 10
DUPLICATE_THRESHOLD = 0.8
SCOPE = ("cmd", "internal")
RAW_PROFILE = Path("target/coverage/profile.out")
PROFILE = Path("target/coverage/coverage.out")
BASELINE = Path(".quality/baseline.json")
MUTANT_TESTS = ".quality/mutant-tests.sh"
# Line 0 never matches: it stands for a whole file missing from the coverage profile, and no waiver covers that.
EQUIVALENT = re.compile(r"Equivalent mutant:\s*(\S+?):([1-9]\d*)\s+(\S.{9,})")
# A bidirectional control could make the reason read differently from what it says, so none may appear anywhere in
# the paragraph, on any of its lines.
BIDI = re.compile(r"[؜‎‏‪-‮⁦-⁩]")
SOURCE = re.compile(r"^(?:cmd|internal)/.+\.go$")
SAFE_PATH = re.compile(r"^[A-Za-z0-9_./-]+$")
# The repo's whole test run takes well under a minute; the limit keeps a stuck base run inside the job's timeout.
BASE_COVERAGE_SECONDS = 240.0
# The mutation budget is time, sized to the change (engineering quality standard): the time already spent, plus the
# rounds each file needs (its sites divided by the workers, rounded up, and one for mutator's baseline run), times the
# measured time of one mutant. Never below the floor, never above the cap.
MUTATION_FLOOR_SECONDS = 300.0
MUTATION_CAP_SECONDS = 1800.0
CPU_MAX = Path("/sys/fs/cgroup/cpu.max")

Ranges = dict[str, list[tuple[int, int]]]


def run(args: list[str], cwd: Path | None = None, check: bool = True, env: Mapping[str, str] | None = None):
    result = subprocess.run(args, cwd=cwd, capture_output=True, encoding="utf-8", env=env)
    if check and result.returncode != 0:
        sys.exit(f"command failed ({result.returncode}): {' '.join(args)}\n{result.stdout[-3000:]}\n{result.stderr[-3000:]}")
    return result


def resolve_base(explicit: str | None) -> str:
    """The commit the PR is measured against.

    A pull_request checkout is GitHub's merge commit, whose first parent is the current base tip, so HEAD^1 is exact
    even when the base moved after the PR was opened. Otherwise use --base.
    """
    if run(["git", "rev-parse", "--verify", "-q", "HEAD^2"], check=False).returncode == 0:
        return run(["git", "rev-parse", "HEAD^1"]).stdout.strip()
    if not explicit:
        sys.exit("HEAD is not a merge commit; pass --base <sha>")
    return run(["git", "rev-parse", "--verify", f"{explicit}^{{commit}}"]).stdout.strip()


def go_file(path: str) -> bool:
    """A Go file under the scope directories, tests included (the format check reads these)."""
    return bool(SOURCE.match(path)) and "testdata" not in path.split("/")


def in_scope(path: str) -> bool:
    return go_file(path) and not path.endswith("_test.go")


def safe_path(path: str) -> bool:
    """A plain repository-relative name: no space, tab, newline, quote or non-ASCII character, and no .. part."""
    return bool(SAFE_PATH.match(path)) and ".." not in path.split("/")


GIT_DIFF = ["git", "--literal-pathspecs", "diff", "--no-ext-diff", "--no-textconv", "--no-renames"]


def added_counts(base: str) -> dict[str, int]:
    """Go file under the scope directories (tests included) -> lines it adds. -z keeps every name whole, as git stores
    it.

    The gate stops on any listed name that is not a safe path, Go or not, and on a Go file it cannot read as text: a
    binary file, a symbolic link (the checks would read its target, which may be outside the repository) or a
    submodule."""
    listed = run([*GIT_DIFF, "--numstat", "-z", "--diff-filter=AMT", base, "HEAD", "--", *SCOPE]).stdout
    counts: dict[str, int] = {}
    for record in filter(None, listed.split("\0")):
        added, _deleted, path = record.split("\t", 2)
        if not safe_path(path):
            sys.exit(f"{path!r} is not a safe repository path: rename it to letters, digits, '_', '-', '.' and '/'")
        if not go_file(path):
            continue
        if added == "-":
            sys.exit(f"git reads {path} as a binary file, so its changed lines cannot be judged")
        mode = run(["git", "--literal-pathspecs", "ls-tree", "HEAD", "--", path]).stdout.split(" ", 1)[0]
        if mode != "100644" and mode != "100755":
            sys.exit(f"{path} is not a regular file (mode {mode}), so its changed lines cannot be judged")
        counts[path] = int(added)  # a pure deletion counts 0 and gets no range
    return counts


def changed_go_files(base: str) -> list[str]:
    """Every Go file under the scope directories that the PR adds or changes, tests included."""
    return sorted(added_counts(base))


class Hunk(NamedTuple):
    """One hunk of a -U0 patch: old start, old count, new start, new count."""

    old_start: int
    old_count: int
    new_start: int
    new_count: int


HUNK = re.compile(r"^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@", re.MULTILINE)


def hunks(diff: str) -> list[Hunk]:
    return [Hunk(int(a), 1 if b == "" else int(b), int(c), 1 if d == "" else int(d)) for a, b, c, d in HUNK.findall(diff)]


def new_line(moved: list[Hunk], old: int) -> int | None:
    """Where an old line is now, or None when the patch changed or deleted it. A hunk that adds only (old count 0)
    sits after its old start line."""
    shift = 0
    for old_start, old_count, _new_start, new_count in moved:
        if old_count and old_start <= old < old_start + old_count:
            return None
        if old_start + max(old_count, 1) - 1 < old:
            shift += new_count - old_count
    return old + shift


def file_diff(base: str, path: str) -> str:
    """The -U0 patch of one file from base to HEAD; the name is a literal path, never a pattern."""
    return run([*GIT_DIFF, "-U0", base, "HEAD", "--", path]).stdout


def collapse(lines: set[int]) -> list[tuple[int, int]]:
    ranges: list[tuple[int, int]] = []
    for number in sorted(lines):
        if ranges and number == ranges[-1][1] + 1:
            ranges[-1] = (ranges[-1][0], number)
        else:
            ranges.append((number, number))
    return ranges


def changed_ranges(base: str) -> Ranges:
    """Added or modified lines (new-file numbering) per in-scope file. Each file's hunks must add up to the lines git
    counts for it; when they do not, the gate stops, because a file it cannot read must never pass as unchanged."""
    found: Ranges = {}
    for path, added in sorted(added_counts(base).items()):
        if not in_scope(path):
            continue
        lines = {n for hunk in hunks(file_diff(base, path)) for n in range(hunk.new_start, hunk.new_start + hunk.new_count)}
        if len(lines) != added:
            sys.exit(f"could not read the changed lines of {path}: git counts {added} added, its hunks do not")
        if lines:
            found[path] = collapse(lines)
    return found


def touches(ranges: list[tuple[int, int]], first: int, last: int) -> bool:
    return any(start <= last and end >= first for start, end in ranges)


def check_format(files: list[str]) -> tuple[bool, list[str]]:
    """gofmt -l on the changed Go files, tests included."""
    if not files:
        return True, ["No Go file was added or changed."]
    result = run(["gofmt", "-l", "--", *files], check=False)
    if result.returncode != 0:
        return False, [f"gofmt failed (exit {result.returncode}):", result.stderr[-3000:]]
    listed = sorted(filter(None, result.stdout.splitlines()))
    if not listed:
        return True, [f"{len(files)} changed Go file(s) are gofmt clean."]
    return False, ["gofmt would change these files; run gofmt -w on them:", *(f"  {path}" for path in listed)]


class Block(NamedTuple):
    """One block of a Go coverage profile: where it starts and ends, its statements and how often it ran."""

    start_line: int
    start_col: int
    end_line: int
    end_col: int
    statements: int
    count: int


PROFILE_BLOCK = re.compile(r"^(.+\.go):(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)$")


def module_path(root: Path) -> str:
    for line in (root / "go.mod").read_text(encoding="utf-8").splitlines():
        if line.startswith("module "):
            return line.split()[1]
    sys.exit("go.mod names no module")


def relative_profile(text: str, module: str) -> str:
    """The profile with each module path turned into a repository path. A block outside the module stops the gate:
    its file could not be found in the checkout."""
    prefix = module + "/"
    lines: list[str] = []
    for line in text.splitlines():
        if line.startswith("mode:"):
            lines.append(line)
        elif line.startswith(prefix):
            lines.append(line[len(prefix):])
        elif line.strip():
            sys.exit(f"the coverage profile has a block outside module {module}: {line[:200]}")
    return "\n".join(lines) + "\n"


def parse_profile(text: str) -> dict[str, list[Block]]:
    """File -> its blocks. A line that is neither the mode nor a block stops the gate rather than being skipped."""
    files: dict[str, list[Block]] = {}
    for line in text.splitlines():
        if line.startswith("mode:") or not line.strip():
            continue
        match = PROFILE_BLOCK.match(line)
        if not match:
            sys.exit(f"cannot read this coverage profile line: {line[:200]}")
        path, *numbers = match.groups()
        files.setdefault(path, []).append(Block(*map(int, numbers)))
    return files


def line_hits(blocks: list[Block]) -> dict[int, bool]:
    """Executable line -> whether a test ran it: a line is executable when a block with a statement spans it, and hit
    when any such block that spans it ran. An empty function is a block of 0 statements, with nothing to run."""
    hits: dict[int, bool] = {}
    for block in blocks:
        if not block.statements:
            continue
        for line in range(block.start_line, block.end_line + 1):
            hits[line] = hits.get(line, False) or block.count > 0
    return hits


def repo_line_coverage(profile: dict[str, list[Block]]) -> float:
    """Statement coverage of the scope, in percent with two decimals, as go tool cover -func totals it. A block listed
    more than once (one package's tests and another's) counts once, as run when any listing ran."""
    statements: dict[tuple[str, int, int, int, int], int] = {}
    ran: dict[tuple[str, int, int, int, int], bool] = {}
    for path, blocks in profile.items():
        if not in_scope(path):
            continue
        for block in blocks:
            key = (path, *block[:4])
            statements[key] = block.statements
            ran[key] = ran.get(key, False) or block.count > 0
    total = sum(statements.values())
    hit = sum(count for key, count in statements.items() if ran[key])
    return round(100.0 * hit / total, 2) if total else 100.0


# Nodes whose body holds statements a test has to run.
FUNCTIONS = frozenset({"function_declaration", "method_declaration", "func_literal"})


def has_code(source: str) -> bool:
    """Whether the file has a function body with a statement in it, or does not parse, so its code cannot be seen. A
    file with neither (types, constants, empty functions) is one the coverage profile leaves out."""
    from tree_sitter_language_pack import get_parser

    root = get_parser("go").parse(source.encode("utf-8")).root_node
    if root.has_error:
        return True
    stack = [root]
    while stack:
        node = stack.pop()
        if node.type in FUNCTIONS:
            body = node.child_by_field_name("body")
            if body is not None and any(child.is_named and child.type != "comment" for child in body.children):
                return True
        stack.extend(node.children)
    return False


# A line with nothing a test could run: blank, a comment, or only braces and parentheses. A block spans such lines
# (the closing brace of an if, a comment inside a body), so they would otherwise count as untested.
INERT = re.compile(r"^\s*(?:[{}()]\s*)*(?://.*)?$")


def uncovered_changed(changed: Ranges, profile: dict[str, list[Block]], read_source: Callable[[str], str | None]) -> list[tuple[str, int]]:
    """Changed executable lines that no test runs: inside a block that never ran, in no block that did, and not inert.
    A file the profile does not list at all is reported as line 0 unless it holds no code (has_code); one that cannot
    be read counts as holding code. No waiver covers line 0."""
    found: list[tuple[str, int]] = []
    for path, ranges in sorted(changed.items()):
        source = read_source(path)
        if path in profile:
            found.extend((path, number) for number in untested_lines(ranges, profile[path], source or ""))
        elif source is None or has_code(source):
            found.append((path, 0))
    return found


def untested_lines(ranges: list[tuple[int, int]], blocks: list[Block], source: str) -> list[int]:
    """The changed lines of one file that a block spans, that no block which ran spans, and that are not inert."""
    text = source.splitlines()

    def inert(number: int) -> bool:
        return number <= len(text) and INERT.match(text[number - 1]) is not None

    hits = line_hits(blocks)
    return [n for n in sorted(hits) if not hits[n] and touches(ranges, n, n) and not inert(n)]


def read_text(path: str) -> str | None:
    try:
        return Path(path).read_text(encoding="utf-8", errors="replace")
    except OSError:
        return None


def coverage_verdict(
    uncovered: list[tuple[str, int]], allowed: dict[tuple[str, int], str], now: float, baseline: dict
) -> tuple[bool, list[str]]:
    """The PR fails on its own untested lines, never on the repo total."""
    floor = baseline["coverage"]["lines"]
    lines = [f"Repo line coverage {now}% (baseline {floor}%), reported only."]
    if now > floor:
        lines.append("Coverage improved: update coverage in .quality/baseline.json in this PR.")
    elif now < floor:
        lines.append("Below the baseline; that alone does not fail the PR (a deletion can cause it).")
    bad = [item for item in uncovered if item not in allowed]
    for path, number in uncovered:
        reason = allowed.get((path, number))
        where = f"{path}:{number}" if number else f"{path} (not in the coverage profile; a test has to build and run it)"
        lines.append(f"  untested changed line {where}" + (f" (explained: {reason})" if reason else ""))
    if bad:
        lines.append(
            "Add a test that runs each line, or, when no test can reach it, add 'Equivalent mutant: <file>:<line> "
            "<reason>' to the PR description as a paragraph of its own."
        )
    else:
        lines.insert(0, "Every executable line the PR adds or changes is run by a test.")
    return not bad, lines


def write_coverage(root: Path, seconds: float | None = None) -> None:
    """Run the tests with coverage under root and write the repo-relative profile. Raises BaseNotScored when the run
    does not finish in time or fails."""
    raw = root / RAW_PROFILE
    raw.parent.mkdir(parents=True, exist_ok=True)
    code, _out, err = run_limited(
        ["go", "test", "-count=1", f"-coverprofile={raw}", "./..."], root, dict(os.environ), seconds or MUTATION_CAP_SECONDS
    )
    if code is None:
        raise BaseNotScored(f"the coverage run did not finish within {seconds:.0f}s")
    if code != 0:
        raise BaseNotScored(f"the coverage run failed (exit {code}):\n{err[-1500:]}")
    relativize(root)


def relativize(root: Path) -> None:
    """Write root/target/coverage/coverage.out from the raw profile go test wrote."""
    text = (root / RAW_PROFILE).read_text(encoding="utf-8")
    (root / PROFILE).write_text(relative_profile(text, module_path(root)), encoding="utf-8")


def check_coverage(root: Path, changed: Ranges, baseline: dict, allowed: dict[tuple[str, int], str]) -> tuple[bool, list[str]]:
    profile = parse_profile((root / PROFILE).read_text(encoding="utf-8"))
    return coverage_verdict(uncovered_changed(changed, profile, read_text), allowed, repo_line_coverage(profile), baseline)


Key = tuple[str, str, int]
"""A function's identity: (path, receiver and name, nth function of that name in the file). A function that moved to
another file or was renamed has no key on base: it is new."""
Scored = tuple[Key, int, float, int, float]
"""One function's score: (key, start line, CRAP, complexity, coverage percent)."""


def qualified(namespace: str, name: str) -> str:
    """A method as Receiver.name, a function as its name: crapper puts the receiver after the package in the
    namespace (".../internal/ghapp.Client")."""
    last = namespace.rsplit("/", 1)[-1]
    receiver = last.split(".", 1)[1] if "." in last else ""
    return f"{receiver}.{name}" if receiver else name


def functions(root: Path, path: str):
    """(key, function) for each function crapper finds in one file."""
    from crapper.languages import functions_in_file

    source = (root / path).read_text(encoding="utf-8", errors="replace")
    seen: Counter[str] = Counter()
    for function in functions_in_file("go", source, path, str(root)):
        name = qualified(function.namespace, function.name)
        yield (path, name, seen[name]), function
        seen[name] += 1


def scored_functions(root: Path, paths: list[str], need_coverage: bool = False) -> list[Scored]:
    """Every function in the given files with its CRAP, from root/target/coverage/coverage.out.

    A function with no coverage record scores as 0% covered, unless `need_coverage`: then it is left out, because 0%
    is the highest score it could have, which would let any head score pass against it."""
    from crapper.coverage import load_bundle
    from crapper.crap import make_entry

    bundle = load_bundle(root)
    bundle.bind_sources(sorted(paths))
    found: list[Scored] = []
    for path in sorted(paths):
        for key, function in functions(root, path):
            percent = bundle.percent_for(function)
            if percent is None and need_coverage:
                continue
            entry = make_entry(function, 0.0 if percent is None else percent)
            if entry.crap is not None:
                found.append((key, function.start_line, entry.crap, entry.complexity, entry.coverage))
    return found


def touched_functions(root: Path, changed: Ranges) -> set[Key]:
    """Functions with a changed line in them."""
    return {
        key
        for path, ranges in changed.items()
        for key, function in functions(root, path)
        if touches(ranges, function.start_line, function.end_line)
    }


def name_counts(keys) -> Counter[tuple[str, str]]:
    """(path, name) -> how many functions of that name the file holds."""
    return Counter((path, name) for path, name, _nth in keys)


def over_limit_problem(crap: float, before: float | None) -> str | None:
    """Why a function at or over the limit fails, or None when it was already over on base and is no worse."""
    if before is None:
        return "new function (no score on base under this file and name)"
    if before < CRAP_LIMIT:
        return f"was {before:.1f} on base (under the limit)"
    if crap > before:
        return f"was {before:.1f} on base (higher)"
    return None


def crap_verdict(head: list[Scored], touched: set[Key], base: dict[Key, float] | None) -> tuple[bool, list[str]]:
    """A function the PR touches passes under the limit. One that is over the limit passes only when it was already
    over on base and scores no higher now (raw scores; one decimal is only how they print); a new, moved or renamed
    one has to be under, and so is every function of a name whose count in its file changed (order cannot tell which
    one is the old one)."""
    base = base or {}
    on_head, on_base = name_counts(key for key, *_ in head), name_counts(base)
    judged = [entry for entry in head if entry[0] in touched]
    bad: list[str] = []
    still: list[str] = []
    for key, line, crap, complexity, coverage in judged:
        if crap < CRAP_LIMIT:
            continue
        label = f"{key[0]}:{line} {key[1]} CRAP {crap:.1f} (complexity {complexity}, coverage {coverage:.0f}%)"
        before = base.get(key) if on_head.get(key[:2]) == on_base.get(key[:2]) else None
        problem = over_limit_problem(crap, before)
        if problem:
            bad.append(f"{label}, {problem}")
        else:
            still.append(f"{label}, base {before:.1f}")
    lines = [f"{len(judged)} added or changed function(s) scored, limit is under {CRAP_LIMIT}."]
    lines += [f"  still over the limit, no worse than base: {item}" for item in still]
    lines += [f"  over the limit: {item}" for item in bad]
    return not bad, lines


class BaseTree:
    """The base commit in a throwaway worktree."""

    def __init__(self, base: str, root: Path) -> None:
        self.base, self.root = base, root
        self.dir = Path(tempfile.mkdtemp(prefix="quality-base-"))
        self.tree = self.dir / "tree"

    def __enter__(self) -> Path:
        run(["git", "worktree", "add", "--detach", str(self.tree), self.base], cwd=self.root)
        return self.tree

    def __exit__(self, *_exc) -> None:
        run(["git", "worktree", "remove", "--force", str(self.tree)], cwd=self.root, check=False)
        shutil.rmtree(self.dir, ignore_errors=True)


class BaseNotScored(Exception):
    """The base coverage run did not finish in time or failed, so no base score is known."""


def base_scores(base: str, root: Path, paths: list[str]) -> dict[Key, float]:
    """CRAP on base for the functions in the given files, from a coverage run in a base worktree within
    BASE_COVERAGE_SECONDS. Raises BaseNotScored when the run does not finish in time or fails."""
    with BaseTree(base, root) as tree:
        existing = [path for path in paths if (tree / path).is_file()]
        if not existing:
            return {}
        write_coverage(tree, BASE_COVERAGE_SECONDS)
        return {key: crap for key, _line, crap, _complexity, _coverage in scored_functions(tree, existing, need_coverage=True)}


def check_crap(changed: Ranges, root: Path, base: str) -> tuple[bool, list[str]]:
    head = scored_functions(root, sorted(changed))
    touched = touched_functions(root, changed)
    over = sorted({key[0] for key, _l, crap, _c, _v in head if key in touched and crap >= CRAP_LIMIT})
    try:
        before = base_scores(base, root, over) if over else None
    except BaseNotScored as reason:
        _ok, lines = crap_verdict(head, touched, {})
        return False, lines + [
            f"Base not scored: {reason}. Re-run this job once; until its base score is known, a function over the "
            "limit is judged as new."
        ]
    return crap_verdict(head, touched, before)


Place = tuple[str, int, int]
"""One side of a duplicate pair: file, first line, last line."""
Pair = tuple[float, Place, Place]


# The start of each block in dryer's report; a line, so a path that holds the word cannot add one.
DUPLICATE = re.compile(r"^DUPLICATE ", re.MULTILINE)


def parse_dryer(out: str) -> list[Pair]:
    pairs: list[Pair] = []
    # The text before the first marker is not a block.
    for block in DUPLICATE.split(out)[1:]:
        score = re.match(r"score=([\d.]+)", block)
        places = re.findall(r"^\s+(\S+):(\d+)-(\d+)$", block, re.MULTILINE)
        if score and len(places) == 2:
            left, right = ((p, int(a), int(b)) for p, a, b in places)
            pairs.append((float(score.group(1)), left, right))
    # A block the gate cannot read could hide a new pair, so it stops the gate rather than being skipped.
    blocks = len(DUPLICATE.findall(out))
    if blocks != len(pairs):
        sys.exit(f"dryer printed {blocks} DUPLICATE blocks but {len(pairs)} pairs could be read:\n{out[-3000:]}")
    return pairs


def dryer_pairs(root: Path) -> list[Pair]:
    dirs = [name for name in SCOPE if (root / name).is_dir()]
    out = run(["dryer", "--threshold", str(DUPLICATE_THRESHOLD), *dirs], cwd=root).stdout if dirs else ""
    shutil.rmtree(root / ".metrics", ignore_errors=True)
    return [pair for pair in parse_dryer(out) if in_scope(pair[1][0]) and in_scope(pair[2][0])]


Stretch = tuple[float, float]
"""Where a head side sits inside a base side: the first and past-the-last fraction of the base side's lines."""


def replaced(moved: list[Hunk], line: int) -> int | None:
    """The old line that a changed line replaced one for one, or None for a line the patch added beyond what it
    removed."""
    for old_start, old_count, new_start, new_count in moved:
        if new_start <= line < new_start + min(old_count, new_count):
            return old_start + line - new_start
    return None


def held(was: Place, now: Place, changed: Ranges, moved: Callable[[str], list[Hunk]]) -> Stretch | None:
    """The stretch of a base side that a head side still holds, or None when the head side is not that base side.
    It is when they are in one file, a base line that survives the diff lands inside it, and every changed line inside
    it is within the base side: between its first and last surviving lines, or replacing one of its lines one for one.
    So edits inside an old side pass, and lines the PR wrote past its ends do not."""
    path, first, last = was
    if path != now[0]:
        return None
    hunks_of_file = moved(path)
    kept = {old: line for old in range(first, last + 1) if (line := new_line(hunks_of_file, old)) is not None}
    inside = [old for old, line in kept.items() if now[1] <= line <= now[2]]
    if not inside:
        return None
    low, high = min(kept.values()), max(kept.values())

    def ours(line: int) -> bool:
        source = replaced(hunks_of_file, line)
        return low <= line <= high or (source is not None and first <= source <= last)

    written = (line for start, end in changed.get(path, []) for line in range(max(start, now[1]), min(end, now[2]) + 1))
    if not all(ours(line) for line in written):
        return None
    size = last - first + 1
    return (min(inside) - first) / size, (max(inside) - first + 1) / size


def lined_up(left: Stretch | None, right: Stretch | None) -> bool:
    """Both sides are held and sit at overlapping places in their base sides, as the two sides of one pair do."""
    return left is not None and right is not None and left[0] < right[1] and right[0] < left[1]


def new_pairs(before: list[Pair], after: list[Pair], changed: Ranges, moved: Callable[[str], list[Hunk]]) -> list[Pair]:
    """Head pairs that touch a changed line and that no base pair between the same two files still holds (held), with
    both sides lined up, so swapping, growing or realigning a duplicate fails. Both sides are tried either way round,
    so a pair inside one file matches whatever order dryer lists it in."""

    def old(pair: Pair) -> bool:
        _score, left, right = pair
        return any(
            lined_up(held(was_l, left, changed, moved), held(was_r, right, changed, moved))
            or lined_up(held(was_l, right, changed, moved), held(was_r, left, changed, moved))
            for _was_score, was_l, was_r in before
        )

    def touched(pair: Pair) -> bool:
        return any(path in changed and touches(changed[path], first, last) for path, first, last in pair[1:])

    return [pair for pair in after if touched(pair) and not old(pair)]


def check_duplicates(changed: Ranges, base: str, root: Path, baseline: dict) -> tuple[bool, list[str]]:
    """A pair is new when it touches a changed line and no base pair still holds the same lines (new_pairs)."""
    with BaseTree(base, root) as tree:
        before = dryer_pairs(tree)
    after = dryer_pairs(root)
    moved = functools.cache(lambda path: hunks(file_diff(base, path)))
    bad = [
        f"{left[0]}:{left[1]}-{left[2]} and {right[0]}:{right[1]}-{right[2]} (similarity {score:.2f})"
        for score, left, right in new_pairs(before, after, changed, moved)
    ]
    lines = [f"{len(after)} duplicate pair(s) at {DUPLICATE_THRESHOLD} or above on this PR, {len(before)} on base."]
    if len(after) < baseline["duplicatePairs"]:
        lines.append("Fewer pairs than the baseline: update duplicatePairs in .quality/baseline.json in this PR.")
    return not bad, lines + [f"  new duplicate: {item}" for item in bad]


# Elements that never hold content, so they never wrap a waiver.
VOID = frozenset({"area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track", "wbr"})


class TopLevelParagraphs(HTMLParser):
    """The text of each top-level <p> in GitHub's rendering that holds text and nothing else. Anything inside another
    element (<details>, <pre>, a list, a quote) or next to markup in its paragraph is left out, and an element that is
    never closed covers everything after it."""

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.open: list[str] = []
        self.text: list[str] | None = None
        self.found: list[str] = []

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        self.text = [] if tag == "p" and not self.open and not attrs else None
        if tag not in VOID:
            self.open.append(tag)

    def handle_startendtag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        self.text = None

    def handle_endtag(self, tag: str) -> None:
        if tag not in self.open:
            return
        while self.open.pop() != tag:
            pass
        # Text is collected only from a top-level <p> with no tag since, so this end tag closes that very paragraph.
        if self.text is not None:
            self.found.append("".join(self.text))
            self.text = None

    def handle_data(self, data: str) -> None:
        if self.text is not None:
            self.text.append(data)


def survivors_allowed(html: str) -> dict[tuple[str, int], str]:
    """The waivers in GitHub's rendering of the description: each must be the whole text of a top-level paragraph, so
    a reviewer always sees it on the PR page."""
    parser = TopLevelParagraphs()
    parser.feed(html)
    parser.close()
    found = (EQUIVALENT.fullmatch(text.strip()) for text in parser.found if not BIDI.search(text))
    return {(m.group(1), int(m.group(2))): m.group(3).strip() for m in found if m}


def read_waivers(path: str | None, sha256: str | None = None) -> tuple[bool, list[str], dict[tuple[str, int], str]]:
    """The waivers that count, from GitHub's rendering of the description, written to `path` by an earlier CI step
    together with its sha256 (a step output, which no later step can change). Without a file nothing is waived; a file
    that cannot be read, comes without its hash or does not match it fails, because then the gate cannot tell which
    waivers a reviewer sees."""
    if path is None:
        return True, ["No rendered description was given: nothing is waived."], {}
    try:
        data = Path(path).read_bytes()
    except OSError as error:
        return False, [f"The rendered description cannot be read, so no waiver can be: {error}"], {}
    if sha256 is None or hashlib.sha256(data).hexdigest() != sha256.strip().lower():
        return False, ["The rendered description does not match the sha256 it was rendered with, so no waiver can be read."], {}
    allowed = survivors_allowed(data.decode("utf-8", errors="replace"))
    lines = [f"{len(allowed)} waiver(s) count, each the whole text of a top-level paragraph on GitHub."]
    lines.extend(f"  {where}:{line}" for where, line in sorted(allowed))
    return True, lines, allowed


def _kill_pass(tag: str, proc_dir: Path = Path("/proc")) -> int:
    """One scan of /proc: kill every process that carries the tag. Returns how many it killed."""
    needle = f"QUALITY_GATE_TAG={tag}".encode()
    killed = 0
    if not proc_dir.is_dir():
        return killed
    for entry in proc_dir.iterdir():
        if not entry.name.isdigit() or int(entry.name) == os.getpid():
            continue
        try:
            if needle in (entry / "environ").read_bytes().split(b"\0"):
                os.kill(int(entry.name), signal.SIGKILL)
                killed += 1
        except OSError:
            continue
    return killed


def kill_tagged(tag: str, passes: int = 20, proc_dir: Path = Path("/proc")) -> None:
    """Kill every process that carries QUALITY_GATE_TAG=<tag> in its environment (Linux, via /proc).

    mutator starts each test run in its own session, so killing mutator's process group leaves go and the test
    binaries running; they inherit the tag from the environment, which is how they are found. A process can fork
    between the scan and the kill, so scan again until a pass finds none.
    """
    for _ in range(passes):
        if _kill_pass(tag, proc_dir) == 0:
            return
        time.sleep(0.05)


def run_limited(args: list[str], cwd: Path, env: dict, seconds: float) -> tuple[int | None, str, str]:
    """Run args; on timeout kill the process group and everything tagged by this run, and return code None."""
    tag = uuid.uuid4().hex
    proc = subprocess.Popen(
        args, cwd=cwd, env={**env, "QUALITY_GATE_TAG": tag}, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        text=True, start_new_session=True,
    )
    try:
        out, err = proc.communicate(timeout=max(seconds, 0.1))
        return proc.returncode, out, err
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid, signal.SIGKILL)
        kill_tagged(tag)
        proc.communicate()
        return None, "", ""


def cpu_workers(cpu_max: Path = CPU_MAX, cores: int | None = None) -> int:
    """Mutants to run at once: the CPU quota of this cgroup when it sets one (a container runner reports the host's
    cores), else the cores."""
    cores = cores or os.cpu_count() or 1
    try:
        quota, period = cpu_max.read_text().split()[:2]
        return max(1, min(cores, int(quota) // int(period)))
    except (OSError, ValueError):
        return cores


class MutatorFailed(Exception):
    """mutator exited with an error rather than a verdict."""


def wanted_lines(ranges: list[tuple[int, int]]) -> list[int]:
    return sorted({n for start, end in ranges for n in range(start, end + 1)})


def mutator_args(path: str, wanted: list[int], *extra: str) -> list[str]:
    return ["mutator", path, "--lines", ",".join(map(str, wanted)), "--use-existing-coverage", *extra]


SCAN_HEADER = re.compile(r"^Scan: (\d+) mutation sites in ", re.MULTILINE)
SCAN_SITE = re.compile(r"[ *] \S+?:(\d+) ")
RESULT = re.compile(r"(KILLED|SURVIVED)\s+(\S+?):(\d+)\s+(.*)")


def scan_sites(path: str, wanted: list[int], root: Path) -> list[int]:
    """The line of each mutation site on the wanted lines (a scan runs no tests). Raises MutatorFailed unless the scan
    exits 0 with one "Scan: N mutation sites" header and N site lines under it.

    Anything else (a crash, a file mutator skips, a changed output format) would otherwise read as a file with nothing
    to mutate, and the check would pass without mutating it. Site lines start with a space, or with * when the
    function changed since mutator's last snapshot.
    """
    code, out, err = run_limited(mutator_args(path, wanted, "--scan"), root, dict(os.environ), 30)
    if code is None:
        raise MutatorFailed(f"mutator --scan did not finish on {path} within 30s")
    headers = SCAN_HEADER.findall(out)
    sites = [int(m.group(1)) for line in out.splitlines() if (m := SCAN_SITE.match(line))]
    if code != 0 or len(headers) != 1 or int(headers[0]) != len(sites):
        raise MutatorFailed(f"mutator --scan failed on {path} (exit {code}):\n{out[-1500:]}\n{err[-1500:]}")
    return sites


Result = tuple[str, str, int, str]  # verdict (KILLED or SURVIVED), path, line, operator


STOPPED = "(run stopped: line of its first changed character)"


def logged_results(path: str, log: Path) -> list[Result]:
    """The verdicts mutant-tests.sh logged for the mutants that finished before a run was stopped. Each names the line
    of the mutant's first changed character, which is its site's line. Code from the pull request can write this
    file, so it only ever feeds the report of a run that already fails."""
    try:
        rows = log.read_text(encoding="utf-8", errors="replace").splitlines()
    except OSError:
        return []
    found: list[Result] = []
    for row in rows:
        line, _tab, verdict = row.partition("\t")
        if line.isdigit() and verdict in ("killed", "survived"):
            found.append((verdict.upper(), path, int(line), STOPPED))
    return found


def mutant_env(path: str, root: Path, log: Path) -> dict[str, str]:
    """The file reaches the test command through the environment, never through the command string."""
    return {**os.environ, "MUTANT_FILE": path, "QUALITY_ROOT": str(root.resolve()), "QUALITY_MUTANT_LOG": str(log)}


def mutate_file(path: str, wanted: list[int], root: Path, deadline: float, log: Path, workers: int) -> tuple[list[Result], bool]:
    """Mutate one file's changed lines. Returns the KILLED and SURVIVED results and whether the file was finished.
    When the budget runs out first, the results are those mutant-tests.sh logged to `log` (mutator prints its own only
    once a file is done). Raises MutatorFailed when mutator itself fails."""
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        return [], False
    code, out, err = run_limited(
        mutator_args(path, wanted, "--mutate-all", "--max-workers", str(workers), "--test-command", f"./{MUTANT_TESTS}"),
        root, mutant_env(path, root, log), remaining,
    )
    if code is None:
        return logged_results(path, log), False
    if code not in (0, 3):
        raise MutatorFailed(f"mutator failed on {path} (exit {code}):\n{out[-1500:]}\n{err[-1500:]}")
    results = [(m.group(1), m.group(2), int(m.group(3)), m.group(4)) for line in out.splitlines() if (m := RESULT.match(line))]
    return results, True


def unreported_sites(path: str, sites: list[int], results: list[Result]) -> list[tuple[str, int]]:
    """Sites the scan found that the run gave no verdict for: mutator marks a site with no coverage UNCOVERED and skips
    it, and a file it cannot read gets no results at all. Each is a mutant nobody checked."""
    missing = Counter(sites) - Counter(line for _verdict, _path, line, _op in results)
    return [(path, line) for line in sorted(missing.elements())]


@dataclass
class Mutated:
    """What a mutation run found: killed count, survivors (path, line, operator), sites with no verdict and files it
    did not finish."""

    killed: int = 0
    survived: list[tuple[str, int, str]] = field(default_factory=list)
    unreported: list[tuple[str, int]] = field(default_factory=list)
    unchecked: list[str] = field(default_factory=list)


def mutate_files(sites: dict[str, list[int]], changed: Ranges, root: Path, deadline: float, logs: Path, workers: int) -> Mutated:
    """Mutate the changed lines of each file that has a mutation site on them, in turn, each with a fresh log under
    `logs`. A file the budget stopped is not finished, but what it got through is kept."""
    found = Mutated()
    for index, path in enumerate(path for path, lines in sites.items() if lines):
        results, finished = mutate_file(path, wanted_lines(changed[path]), root, deadline, logs / f"{index}.log", workers)
        found.killed += sum(verdict == "KILLED" for verdict, *_rest in results)
        found.survived.extend((p, line, op) for verdict, p, line, op in results if verdict == "SURVIVED")
        if finished:
            found.unreported.extend(unreported_sites(path, sites[path], results))
        else:
            found.unchecked.append(path)
    return found


def explained(label: str, path: str, line: int, detail: str, allowed: dict[tuple[str, int], str]) -> str:
    reason = allowed.get((path, line))
    tag = f"explained as equivalent: {reason}" if reason else "NOT explained"
    return f"  {label} {path}:{line} {detail}({tag})"


def unexplained(found: Mutated, allowed: dict[tuple[str, int], str]) -> list[tuple[str, int]]:
    spots = [(path, line) for path, line, _op in found.survived] + found.unreported
    return [spot for spot in spots if spot not in allowed]


def survivor_lines(found: Mutated, allowed: dict[tuple[str, int], str]) -> list[str]:
    lines = [explained("survivor", path, line, f"{op} ", allowed) for path, line, op in found.survived]
    lines += [explained("no result", path, line, "", allowed) for path, line in found.unreported]
    if found.unreported:
        lines.append(
            "A site with no result was never mutated: mutator gives no verdict for a site no test covers. Cover the "
            "line, or explain it with an 'Equivalent mutant:' paragraph as for a survivor."
        )
    if unexplained(found, allowed):
        lines.append(
            "Kill each survivor with a test, or add 'Equivalent mutant: <file>:<line> <why it cannot change behaviour>' "
            "to the PR description as a paragraph of its own (one waives every mutant on that line), then re-run this "
            "job."
        )
    return lines


@dataclass(frozen=True)
class Budget:
    """The mutation budget in seconds, the inputs it was sized from (for the report), and whether the change wanted
    more than the cap."""

    seconds: float
    inputs: str
    capped: bool = False


def mutation_budget(spent: float, sites: dict[str, list[int]], per_mutant: float, workers: int) -> Budget:
    """Size the budget: `spent`, plus for each file with sites the rounds its mutants need (sites divided by workers,
    rounded up) and one more for the baseline run mutator makes per file, times `per_mutant`; held to the floor and
    the cap."""
    rounds = sum(-(-len(found) // workers) + 1 for found in sites.values() if found)
    wanted = spent + rounds * per_mutant
    seconds = min(max(wanted, MUTATION_FLOOR_SECONDS), MUTATION_CAP_SECONDS)
    inputs = (
        f"Budget {seconds:.0f}s: {spent:.0f}s spent + {rounds} round(s) x {per_mutant:.0f}s per mutant = {wanted:.0f}s, "
        f"held to {MUTATION_FLOOR_SECONDS:.0f}-{MUTATION_CAP_SECONDS:.0f}s ({workers} worker(s); a round per "
        "worker-full of sites and one per file for mutator's baseline)."
    )
    return Budget(seconds, inputs, wanted > MUTATION_CAP_SECONDS)


def measure_mutant(path: str, root: Path, log: Path) -> float:
    """Time one run of the mutant test command on the unchanged file, which logs nothing. Raises MutatorFailed when
    the tests fail or take longer than the cap."""
    started = time.monotonic()
    code, out, err = run_limited([f"./{MUTANT_TESTS}"], root, mutant_env(path, root, log), MUTATION_CAP_SECONDS)
    if code is None:
        raise MutatorFailed(
            f"One run of the tests did not finish within {MUTATION_CAP_SECONDS:.0f}s, so no mutant can: make the tests faster."
        )
    if code != 0:
        raise MutatorFailed(f"The tests fail before any mutation (exit {code}):\n{out[-1500:]}\n{err[-1500:]}")
    return time.monotonic() - started


def measured_budget(sites: dict[str, list[int]], root: Path, started: float, logs: Path) -> Budget:
    """Measure one mutant once (one stack: Go), on the first file with sites, and size the budget from it."""
    first = next((path for path, found in sites.items() if found), None)
    if first is None:
        return Budget(MUTATION_FLOOR_SECONDS, "No mutation sites, so nothing was measured.")
    per_mutant = measure_mutant(first, root, logs / "measure.log")
    return mutation_budget(time.monotonic() - started, sites, per_mutant, cpu_workers())


def unchecked_lines(unchecked: list[str], sites: dict[str, list[int]], budget: Budget) -> list[str]:
    if not unchecked:
        return []
    if budget.capped:
        advice = (
            f"This change needs more than the {MUTATION_CAP_SECONDS / 60:.0f}-minute cap: split it into smaller PRs, "
            "or make its tests faster."
        )
    else:
        advice = (
            "The runner may have been busy: re-run this job once before doing anything else. A change that still "
            f"cannot finish within {MUTATION_CAP_SECONDS / 60:.0f} minutes is split into smaller PRs, or its tests are "
            "made faster."
        )
    return [
        f"The {budget.seconds:.0f}s mutation budget ran out before every file was checked. Not checked, or not finished:",
        *(f"  {path} ({len(sites[path])} mutation site(s))" for path in unchecked),
        advice,
    ]


Sizer = Callable[[dict[str, list[int]], Path, float, Path], Budget]


def check_mutation(
    changed: Ranges, root: Path, allowed: dict[tuple[str, int], str], size: Sizer = measured_budget
) -> tuple[bool, list[str]]:
    """Mutate the changed lines, file by file, inside a time budget sized to the change (`size`).

    When the budget runs out the job fails cleanly: the report keeps what was checked, names what was not and says
    what to do, instead of the runner killing the job and the report with it.
    """
    shutil.rmtree(root / ".metrics", ignore_errors=True)
    logs = Path(tempfile.mkdtemp(prefix="quality-mutants-"))
    started = time.monotonic()
    workers = cpu_workers()
    try:
        sites = {path: scan_sites(path, wanted_lines(changed[path]), root) for path in sorted(changed)}
        budget = size(sites, root, started, logs)
        found = mutate_files(sites, changed, root, started + budget.seconds, logs, workers)
    except MutatorFailed as failure:
        return False, [str(failure)]
    finally:
        shutil.rmtree(logs, ignore_errors=True)
        shutil.rmtree(root / "target" / "mutation-workers", ignore_errors=True)
        shutil.rmtree(root / ".metrics", ignore_errors=True)

    elapsed = time.monotonic() - started
    counted = sum(map(len, sites.values()))
    files = sum(1 for found_sites in sites.values() if found_sites)
    lines = [
        f"{found.killed} mutant(s) killed, {len(found.survived)} survived on the changed lines, "
        f"{elapsed:.0f}s of a {budget.seconds:.0f}s budget ({counted} site(s) in {files} file(s), {workers} worker(s)).",
        budget.inputs,
    ]
    lines += survivor_lines(found, allowed)
    lines += unchecked_lines(found.unchecked, sites, budget)
    return not unexplained(found, allowed) and not found.unchecked, lines


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--base", help="base commit when HEAD is not a merge commit")
    parser.add_argument(
        "--description-html", help="GitHub's rendering of the PR description, written by an earlier step; no waivers without it"
    )
    parser.add_argument("--description-sha256", help="the sha256 of that file, as the step that wrote it gave it")
    parser.add_argument("--skip-mutation", action="store_true", help="local runs only")
    opts = parser.parse_args()

    root = Path.cwd()
    baseline = json.loads(BASELINE.read_text())
    base = resolve_base(opts.base)
    relativize(root)
    changed = changed_ranges(base)
    ok, lines, allowed = read_waivers(opts.description_html, opts.description_sha256)

    results: list[tuple[str, bool, list[str]]] = [("Waivers", ok, lines)]
    results.append(("Format", *check_format(changed_go_files(base))))
    results.append(("Coverage", *check_coverage(root, changed, baseline, allowed)))
    if not changed:
        results.append(("CRAP, duplication, mutation", True, ["No lines were added or changed in Go source under cmd/ or internal/."]))
    else:
        results.append(("CRAP", *check_crap(changed, root, base)))
        results.append(("Duplication", *check_duplicates(changed, base, root, baseline)))
        if opts.skip_mutation:
            results.append(("Mutation", True, ["Skipped (--skip-mutation)."]))
        elif not all(passed for _, passed, _ in results):
            results.append(("Mutation", False, ["Not run: an earlier check failed. Fix it and re-run this job."]))
        else:
            results.append(("Mutation", *check_mutation(changed, root, allowed)))

    report = [f"## Quality gate (base {base[:8]})", ""]
    for name, passed, check_lines in results:
        report.append(f"### {name}: {'pass' if passed else 'FAIL'}")
        report.extend(check_lines)
        report.append("")
    text = "\n".join(report)
    print(text)
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a", encoding="utf-8") as handle:
            handle.write(text + "\n")
    return 0 if all(passed for _, passed, _ in results) else 1


if __name__ == "__main__":
    sys.exit(main())
