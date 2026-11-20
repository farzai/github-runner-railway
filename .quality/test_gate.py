"""Self-tests for gate.py. Run from the repository root:  python -m unittest discover -s .quality -p 'test_*.py'"""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import textwrap
import time
import unittest
from pathlib import Path
from unittest import mock

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import gate  # noqa: E402

LIB = "internal/core/core.go"
MODULE = "example.com/acme/app"
HERE = Path(__file__).resolve().parent


def fixed(seconds: float):
    """A mutation budget sizer that measures nothing, so a test controls the budget."""
    return lambda *_args: gate.Budget(seconds, f"Budget {seconds:.0f}s, fixed by the test.")


def git(repo: Path, *args: str) -> str:
    env = {
        **os.environ,
        "GIT_AUTHOR_NAME": "t",
        "GIT_AUTHOR_EMAIL": "t@t",
        "GIT_COMMITTER_NAME": "t",
        "GIT_COMMITTER_EMAIL": "t@t",
    }
    return subprocess.run(["git", *args], cwd=repo, env=env, check=True, capture_output=True, text=True).stdout.strip()


def go_source(lines: int = 10) -> str:
    return "package core\n" + "".join(f"const Line{n} = {n}\n" for n in range(2, lines + 1))


class InRepo(unittest.TestCase):
    """A throwaway repository with internal/core/core.go committed; tests change it and diff against the first commit."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.repo = Path(self._tmp.name)
        self._cwd = os.getcwd()
        os.chdir(self.repo)
        git(self.repo, "init", "-q")
        self.write("go.mod", f"module {MODULE}\n\ngo 1.24\n")
        self.write(LIB, go_source())
        self.commit()
        self.base = git(self.repo, "rev-parse", "HEAD")

    def tearDown(self) -> None:
        os.chdir(self._cwd)
        self._tmp.cleanup()

    def write(self, name: str, text: str) -> None:
        path = self.repo / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)

    def lines(self) -> list[str]:
        return (self.repo / LIB).read_text().splitlines()

    def set_lines(self, lines: list[str]) -> None:
        self.write(LIB, "\n".join(lines) + "\n")

    def commit(self) -> None:
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-q", "-m", "change")


class Scope(unittest.TestCase):
    def test_go_source_under_cmd_and_internal_is_in_scope(self) -> None:
        for path in ("cmd/controller/main.go", "internal/ghapp/ghapp.go", "internal/a/b/c.go"):
            with self.subTest(path=path):
                self.assertTrue(gate.in_scope(path))

    def test_tests_testdata_and_other_trees_are_out(self) -> None:
        for path in (
            "internal/ghapp/ghapp_test.go", "internal/ghapp/testdata/x.go", "examples/derived/main.go",
            "internal/ghapp/README.md", "image/x.go", "main.go",
        ):
            with self.subTest(path=path):
                self.assertFalse(gate.in_scope(path))


class ChangedRanges(InRepo):
    def test_a_file_outside_the_scope_is_ignored(self) -> None:
        self.write("internal/core/core_test.go", "package core\n")
        self.write("examples/derived/main.go", "package main\n")
        self.commit()
        self.assertEqual(gate.changed_ranges(self.base), {})

    def test_deleting_the_first_line_judges_nothing(self) -> None:
        self.set_lines(self.lines()[1:])
        self.commit()
        self.assertEqual(gate.changed_ranges(self.base), {})

    def test_deleting_a_lower_line_does_not_mark_the_line_above(self) -> None:
        lines = self.lines()
        del lines[5]
        self.set_lines(lines)
        self.commit()
        self.assertEqual(gate.changed_ranges(self.base), {})

    def test_an_added_line_is_judged_even_next_to_a_deletion(self) -> None:
        lines = self.lines()
        lines[4] = "const Changed = 5"
        del lines[6]
        lines.append("const Added = 11")
        self.set_lines(lines)
        self.commit()
        self.assertEqual(gate.changed_ranges(self.base), {LIB: [(5, 5), (10, 10)]})

    def test_a_new_file_is_judged_whole(self) -> None:
        self.write("cmd/tool/main.go", "package main\n\nfunc main() {}\n")
        self.commit()
        self.assertEqual(gate.changed_ranges(self.base), {"cmd/tool/main.go": [(1, 3)]})

    def test_a_file_name_that_is_not_a_safe_path_stops_the_gate(self) -> None:
        self.write("internal/core/odd name.go", "package core\n")
        self.commit()
        with self.assertRaises(SystemExit) as raised:
            gate.changed_ranges(self.base)
        self.assertIn("not a safe repository path", str(raised.exception))

    def test_a_binary_source_file_stops_the_gate(self) -> None:
        (self.repo / "internal/core/blob.go").write_bytes(b"\0\1\2package")
        self.commit()
        with self.assertRaises(SystemExit) as raised:
            gate.changed_ranges(self.base)
        self.assertIn("binary", str(raised.exception))

    def test_a_symlinked_source_file_stops_the_gate(self) -> None:
        os.symlink("core.go", self.repo / "internal/core/link.go")
        self.commit()
        with self.assertRaises(SystemExit) as raised:
            gate.changed_ranges(self.base)
        self.assertIn("not a regular file", str(raised.exception))

    def test_hunks_that_do_not_add_up_to_gits_count_stop_the_gate(self) -> None:
        self.set_lines(self.lines() + ["const Added = 11"])
        self.commit()
        with mock.patch.object(gate, "file_diff", return_value=""), self.assertRaises(SystemExit) as raised:
            gate.changed_ranges(self.base)
        self.assertIn("could not read the changed lines", str(raised.exception))

    def test_changed_go_files_include_tests_for_the_format_check(self) -> None:
        self.write("internal/core/core_test.go", "package core\n")
        self.set_lines(self.lines() + ["const Added = 11"])
        self.commit()
        self.assertEqual(gate.changed_go_files(self.base), ["internal/core/core.go", "internal/core/core_test.go"])


@unittest.skipUnless(shutil.which("gofmt"), "gofmt is not installed")
class Format(InRepo):
    def test_a_badly_formatted_changed_file_fails(self) -> None:
        self.write("internal/core/bad.go", "package core\nfunc  F( ) {  }\n")
        self.commit()
        ok, lines = gate.check_format(gate.changed_go_files(self.base))
        self.assertFalse(ok)
        self.assertIn("  internal/core/bad.go", lines)

    def test_formatted_files_pass(self) -> None:
        self.write("internal/core/good.go", "package core\n\nfunc F() {}\n")
        self.commit()
        ok, lines = gate.check_format(gate.changed_go_files(self.base))
        self.assertTrue(ok, lines)

    def test_no_changed_file_passes(self) -> None:
        self.assertEqual(gate.check_format([]), (True, ["No Go file was added or changed."]))


PROFILE = textwrap.dedent(
    f"""\
    mode: atomic
    {MODULE}/internal/core/core.go:3.20,5.2 2 1
    {MODULE}/internal/core/core.go:5.2,7.3 1 0
    {MODULE}/internal/core/core.go:9.1,9.20 1 0
    {MODULE}/internal/core/core.go:9.1,9.20 1 4
    {MODULE}/cmd/tool/main.go:3.13,4.2 1 0
    """
)


class Profile(unittest.TestCase):
    def test_module_paths_become_repository_paths(self) -> None:
        text = gate.relative_profile(PROFILE, MODULE)
        self.assertTrue(text.startswith("mode: atomic\ninternal/core/core.go:3.20,5.2 2 1\n"), text)
        self.assertNotIn(MODULE, text)

    def test_a_block_outside_the_module_stops_the_gate(self) -> None:
        with self.assertRaises(SystemExit) as raised:
            gate.relative_profile(PROFILE + "other.org/x/y.go:1.1,2.2 1 1\n", MODULE)
        self.assertIn("other.org/x/y.go", str(raised.exception))

    def test_a_line_that_is_not_a_block_stops_the_gate(self) -> None:
        with self.assertRaises(SystemExit):
            gate.parse_profile(PROFILE + "garbage\n")

    def test_blocks_are_read_per_file(self) -> None:
        blocks = gate.parse_profile(gate.relative_profile(PROFILE, MODULE))
        self.assertEqual(sorted(blocks), ["cmd/tool/main.go", LIB])
        self.assertIn(gate.Block(3, 20, 5, 2, 2, 1), blocks[LIB])

    def test_a_line_is_hit_when_any_block_on_it_ran(self) -> None:
        hits = gate.line_hits(gate.parse_profile(gate.relative_profile(PROFILE, MODULE))[LIB])
        self.assertEqual(hits, {3: True, 4: True, 5: True, 6: False, 7: False, 9: True})

    def test_a_block_without_statements_is_not_executable(self) -> None:
        # go test lists an empty function as a block of 0 statements; there is nothing in it to run.
        self.assertEqual(gate.line_hits([gate.Block(3, 20, 3, 22, 0, 0)]), {})

    def test_repo_coverage_counts_statements_once_in_scope(self) -> None:
        blocks = gate.parse_profile(gate.relative_profile(PROFILE, MODULE))
        # core.go: 2 hit + 1 missed + 1 (the duplicate block, hit once) ; main.go: 1 missed -> 3 of 5
        self.assertEqual(gate.repo_line_coverage(blocks), 60.0)

    def test_repo_coverage_leaves_out_files_outside_the_scope(self) -> None:
        blocks = {"examples/derived/main.go": [gate.Block(1, 1, 2, 2, 5, 0)], LIB: [gate.Block(1, 1, 2, 2, 1, 1)]}
        self.assertEqual(gate.repo_line_coverage(blocks), 100.0)

    def test_the_module_path_comes_from_go_mod(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, "go.mod").write_text(f"// comment\nmodule {MODULE}\n\ngo 1.24\n")
            self.assertEqual(gate.module_path(Path(directory)), MODULE)


CODE = "package core\n\nfunc F(a int) int {\n\treturn a + 1\n}\n"
NO_CODE = "package core\n\ntype T struct{ A int }\n\nconst C = 1\n\nfunc Empty() {}\n\ntype I interface{ M() }\n"


class UncoveredChangedLines(unittest.TestCase):
    def blocks(self) -> dict[str, list[gate.Block]]:
        return gate.parse_profile(gate.relative_profile(PROFILE, MODULE))

    def test_an_executable_changed_line_with_no_hits_is_reported(self) -> None:
        found = gate.uncovered_changed({LIB: [(4, 7)]}, self.blocks(), lambda _p: CODE)
        self.assertEqual(found, [(LIB, 6), (LIB, 7)])

    def test_blank_comment_and_brace_lines_inside_a_missed_block_are_not_judged(self) -> None:
        source = "package core\n\nfunc F() {\n\tif x {\n\n\t\t// why\n\t\ty()\n\t}\n}\n"
        blocks = {LIB: [gate.Block(4, 7, 8, 3, 1, 0)]}
        self.assertEqual(gate.uncovered_changed({LIB: [(4, 8)]}, blocks, lambda _p: source), [(LIB, 4), (LIB, 7)])

    def test_lines_outside_every_block_are_not_executable(self) -> None:
        self.assertEqual(gate.uncovered_changed({LIB: [(1, 2), (8, 8)]}, self.blocks(), lambda _p: CODE), [])

    def test_a_file_missing_from_the_profile_with_code_fails_as_line_0(self) -> None:
        found = gate.uncovered_changed({"internal/x/x.go": [(1, 5)]}, self.blocks(), lambda _p: CODE)
        self.assertEqual(found, [("internal/x/x.go", 0)])

    def test_a_file_missing_from_the_profile_without_code_passes(self) -> None:
        self.assertEqual(gate.uncovered_changed({"internal/x/x.go": [(1, 5)]}, self.blocks(), lambda _p: NO_CODE), [])

    def test_an_unreadable_missing_file_counts_as_code(self) -> None:
        self.assertEqual(gate.uncovered_changed({"internal/x/x.go": [(1, 1)]}, self.blocks(), lambda _p: None),
                         [("internal/x/x.go", 0)])

    def test_has_code_sees_function_bodies_and_function_literals(self) -> None:
        self.assertTrue(gate.has_code(CODE))
        self.assertTrue(gate.has_code("package core\n\nvar f = func() int { return 1 }\n"))
        self.assertTrue(gate.has_code("package core\n\nfunc (t T) M() { t.n++ }\n"))
        self.assertFalse(gate.has_code(NO_CODE))

    def test_a_file_that_does_not_parse_counts_as_code(self) -> None:
        self.assertTrue(gate.has_code("package core\n\nfunc {{{\n"))


class CoverageVerdict(unittest.TestCase):
    BASELINE = {"coverage": {"lines": 80.0}}

    def test_untested_lines_fail_and_the_total_is_only_reported(self) -> None:
        ok, lines = gate.coverage_verdict([(LIB, 6)], {}, 70.0, self.BASELINE)
        self.assertFalse(ok)
        text = "\n".join(lines)
        self.assertIn("Repo line coverage 70.0% (baseline 80.0%)", text)
        self.assertIn(f"untested changed line {LIB}:6", text)

    def test_a_waived_line_passes(self) -> None:
        ok, lines = gate.coverage_verdict([(LIB, 6)], {(LIB, 6): "reached only on a real signal"}, 80.0, self.BASELINE)
        self.assertTrue(ok, lines)
        self.assertIn("explained: reached only on a real signal", "\n".join(lines))

    def test_a_higher_total_asks_for_a_baseline_update(self) -> None:
        ok, lines = gate.coverage_verdict([], {}, 81.0, self.BASELINE)
        self.assertTrue(ok)
        self.assertIn("update coverage in .quality/baseline.json", "\n".join(lines))

    def test_a_missing_file_names_the_report_not_a_line(self) -> None:
        ok, lines = gate.coverage_verdict([("internal/x/x.go", 0)], {}, 80.0, self.BASELINE)
        self.assertFalse(ok)
        self.assertIn("internal/x/x.go (not in the coverage profile", "\n".join(lines))


RENDERED = json.loads((HERE / "rendered-descriptions.json").read_text(encoding="utf-8"))
SAME = "the two branches return the same value"


def html(case: str) -> str:
    return RENDERED[case]["html"]


class Waivers(unittest.TestCase):
    """A waiver counts only as the whole text of a top-level paragraph in GitHub's rendering of the description."""

    def lines(self, case: str) -> list[int]:
        return sorted(line for _path, line in gate.survivors_allowed(html(case)))

    def test_a_plain_paragraph_counts_with_its_reason(self) -> None:
        self.assertEqual(gate.survivors_allowed(html("plain")), {(LIB, 3): SAME})

    def test_each_paragraph_is_one_waiver(self) -> None:
        self.assertEqual(self.lines("two_waivers"), [3, 9])

    def test_a_short_reason_is_not_a_waiver(self) -> None:
        self.assertEqual(gate.survivors_allowed(f"<p>Equivalent mutant: {LIB}:2 because</p>"), {})

    def test_line_0_is_never_a_waiver(self) -> None:
        self.assertEqual(gate.survivors_allowed(f"<p>Equivalent mutant: {LIB}:0 {SAME}</p>"), {})

    def test_hidden_or_wrapped_waivers_do_not_count(self) -> None:
        for case in (
            "tilde_fence", "unclosed_fence", "unclosed_comment", "markup_before", "link_reference_title",
            "multi_line_alt", "attribute_across_blank_lines", "continues_paragraph", "indented",
            "backtick_in_info_string", "fence_inside_html_block", "details", "details_open", "blockquote_and_list",
            "mention_in_reason", "bidi_control", "unclosed_element",
        ):
            with self.subTest(case=case):
                self.assertEqual(self.lines(case), [])

    def test_only_the_visible_paragraph_counts_next_to_hidden_ones(self) -> None:
        for case in ("backtick_fence", "comment", "crlf"):
            with self.subTest(case=case):
                self.assertEqual(self.lines(case), [3])

    def test_a_paragraph_github_shows_counts_even_after_escaped_markup(self) -> None:
        self.assertEqual(self.lines("hidden_elements"), [3, 4])

    def test_lines_joined_into_one_paragraph_do_not_count(self) -> None:
        self.assertEqual(self.lines("adjacent"), [5])

    def test_a_bidi_control_on_any_line_waives_nothing(self) -> None:
        self.assertEqual(gate.survivors_allowed(f"<p>Equivalent mutant: {LIB}:3\nthe two ‮branches return the same</p>"), {})
        self.assertEqual(gate.survivors_allowed(f"<p>Equivalent mutant: {LIB}:3\n{SAME}</p>"), {(LIB, 3): SAME})

    def test_entities_are_read_as_the_text_they_show(self) -> None:
        self.assertEqual(gate.survivors_allowed(html("entities")), {(LIB, 3): "a < b && c > d holds on both sides"})

    def test_a_stray_end_tag_after_a_waiver_adds_nothing(self) -> None:
        self.assertEqual(gate.survivors_allowed(html("stray_end_tag")), {(LIB, 3): SAME})

    def rendered(self, text: str) -> tuple[str, str]:
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        path = Path(directory.name) / "description.html"
        path.write_text(text, encoding="utf-8")
        return str(path), hashlib.sha256(path.read_bytes()).hexdigest()

    def test_the_rendered_description_file_is_read_when_given(self) -> None:
        ok, lines, allowed = gate.read_waivers(*self.rendered(html("plain")))
        self.assertTrue(ok)
        self.assertEqual(allowed, {(LIB, 3): SAME})
        self.assertEqual(lines, ["1 waiver(s) count, each the whole text of a top-level paragraph on GitHub.", f"  {LIB}:3"])

    def test_a_rendered_description_that_changed_after_rendering_fails(self) -> None:
        path, digest = self.rendered(html("plain"))
        Path(path).write_text(html("two_waivers"), encoding="utf-8")
        ok, lines, allowed = gate.read_waivers(path, digest)
        self.assertEqual((ok, allowed), (False, {}))
        self.assertIn("does not match the sha256", lines[0])

    def test_a_rendered_description_without_its_hash_fails(self) -> None:
        path, _digest = self.rendered(html("plain"))
        self.assertEqual(gate.read_waivers(path, None)[:1], (False,))

    def test_a_missing_rendered_description_fails(self) -> None:
        ok, lines, allowed = gate.read_waivers("/nonexistent/description.html", "0" * 64)
        self.assertEqual((ok, allowed), (False, {}))
        self.assertIn("cannot be read", lines[0])

    def test_without_a_rendered_description_nothing_is_waived(self) -> None:
        self.assertEqual(gate.read_waivers(None, None), (True, ["No rendered description was given: nothing is waived."], {}))


def scored(name: str, crap: float, nth: int = 0, path: str = LIB) -> gate.Scored:
    return ((path, name, nth), 1, crap, 3, 50.0)


class CrapVerdict(unittest.TestCase):
    def test_a_touched_function_under_the_limit_passes(self) -> None:
        ok, _ = gate.crap_verdict([scored("a", 9.9)], {(LIB, "a", 0)}, None)
        self.assertTrue(ok)

    def test_a_new_function_at_the_limit_fails(self) -> None:
        ok, lines = gate.crap_verdict([scored("a", 10.0)], {(LIB, "a", 0)}, {})
        self.assertFalse(ok)
        self.assertIn("new function", "\n".join(lines))

    def test_an_untouched_function_over_the_limit_is_not_gated(self) -> None:
        ok, _ = gate.crap_verdict([scored("a", 30.0)], set(), None)
        self.assertTrue(ok)

    def test_an_old_function_over_the_limit_passes_when_no_worse(self) -> None:
        ok, lines = gate.crap_verdict([scored("a", 12.0)], {(LIB, "a", 0)}, {(LIB, "a", 0): 12.0})
        self.assertTrue(ok)
        self.assertEqual(
            lines[1],
            f"  still over the limit, no worse than base: {LIB}:1 a CRAP 12.0 (complexity 3, coverage 50%), base 12.0",
        )

    def test_an_old_function_over_the_limit_fails_when_worse(self) -> None:
        ok, lines = gate.crap_verdict([scored("a", 12.5)], {(LIB, "a", 0)}, {(LIB, "a", 0): 12.0})
        self.assertFalse(ok)
        self.assertIn("was 12.0 on base (higher)", "\n".join(lines))

    def test_a_function_that_was_under_the_limit_must_stay_under(self) -> None:
        ok, lines = gate.crap_verdict([scored("a", 11.0)], {(LIB, "a", 0)}, {(LIB, "a", 0): 8.0})
        self.assertFalse(ok)
        self.assertIn("under the limit", "\n".join(lines))

    def test_a_changed_count_of_same_named_functions_counts_as_new(self) -> None:
        head = [scored("new", 12.0, 0), scored("new", 3.0, 1)]
        ok, _ = gate.crap_verdict(head, {(LIB, "new", 0)}, {(LIB, "new", 0): 20.0})
        self.assertFalse(ok)


class FunctionNames(unittest.TestCase):
    def test_a_method_is_named_by_its_receiver(self) -> None:
        self.assertEqual(gate.qualified("github.com/acme/app/internal/ghapp.Client", "call"), "Client.call")

    def test_a_function_is_named_alone(self) -> None:
        self.assertEqual(gate.qualified("github.com/acme/app/internal/config", "Load"), "Load")


@unittest.skipUnless(shutil.which("go"), "go is not installed")
class CrapOnGo(InRepo):
    """crapper scores Go functions from the repo-relative profile the gate writes."""

    def test_functions_are_scored_and_keyed_by_receiver(self) -> None:
        self.write(LIB, "package core\n\ntype T struct{}\n\nfunc (T) F(a int) int {\n\tif a > 1 {\n\t\treturn 1\n\t}\n\treturn 0\n}\n\nfunc G() {}\n")
        self.write("internal/core/core_test.go", "package core\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {\n\tif (T{}).F(2) != 1 {\n\t\tt.Fatal()\n\t}\n}\n")
        gate.write_coverage(self.repo)
        found = {key: (crap, complexity, coverage) for key, _line, crap, complexity, coverage in gate.scored_functions(self.repo, [LIB])}
        self.assertEqual(sorted(found), [(LIB, "G", 0), (LIB, "T.F", 0)])
        self.assertEqual(found[(LIB, "T.F", 0)][1], 2)
        self.assertLess(found[(LIB, "T.F", 0)][2], 100.0)

    def test_touched_functions_are_those_with_a_changed_line(self) -> None:
        self.write(LIB, "package core\n\nfunc A() {}\n\nfunc B() {\n}\n")
        self.assertEqual(gate.touched_functions(self.repo, {LIB: [(5, 5)]}), {(LIB, "B", 0)})


class DuplicatePairs(unittest.TestCase):
    A, B, C = "internal/core/a.go", "internal/core/b.go", "internal/other/c.go"
    OUTPUT = textwrap.dedent(
        """\
        DUPLICATE score=0.91 kind=function
          internal/core/a.go:3-9
          internal/core/b.go:10-16
        DUPLICATE score=0.85 kind=function
          internal/core/a.go:20-30
          internal/other/c.go:1-11
        """
    )

    @staticmethod
    def no_moves(_path: str) -> list[gate.Hunk]:
        return []

    def test_the_output_is_parsed_into_pairs(self) -> None:
        pairs = gate.parse_dryer(self.OUTPUT)
        self.assertEqual(pairs[0], (0.91, (self.A, 3, 9), (self.B, 10, 16)))
        self.assertEqual(len(pairs), 2)

    def test_a_block_that_cannot_be_read_stops_the_gate(self) -> None:
        block = "DUPLICATE score=0.9\n  internal/a/x.go:1-2\n"
        with self.assertRaises(SystemExit) as raised:
            gate.parse_dryer(self.OUTPUT + block)
        self.assertIn("3 DUPLICATE blocks but 2 pairs", str(raised.exception))

    def test_a_new_pair_on_changed_lines_is_new(self) -> None:
        after = gate.parse_dryer(self.OUTPUT)
        bad = gate.new_pairs([], after, {self.B: [(12, 12)]}, self.no_moves)
        self.assertEqual([pair[0] for pair in bad], [0.91])

    def test_a_pair_that_was_already_on_base_is_not_new(self) -> None:
        after = gate.parse_dryer(self.OUTPUT)
        self.assertEqual(gate.new_pairs(after[:1], after, {self.B: [(12, 12)]}, self.no_moves), [])

    def test_a_pair_listed_the_other_way_round_is_the_same_pair(self) -> None:
        score, left, right = gate.parse_dryer(self.OUTPUT)[0]
        self.assertEqual(gate.new_pairs([(score, right, left)], [(score, left, right)], {self.B: [(12, 12)]}, self.no_moves), [])

    def test_a_pair_away_from_the_changed_lines_is_not_judged(self) -> None:
        after = gate.parse_dryer(self.OUTPUT)
        self.assertEqual(gate.new_pairs([], after, {self.B: [(40, 41)]}, self.no_moves), [])

    def test_removing_one_pair_and_adding_another_between_the_same_files_fails(self) -> None:
        before = [(0.9, (self.A, 1, 5), (self.B, 1, 5))]
        after = [(0.9, (self.A, 20, 25), (self.B, 20, 25))]
        moved = {self.B: [(19, 0, 20, 6)]}
        bad = gate.new_pairs(before, after, {self.B: [(20, 25)]}, lambda path: moved.get(path, []))
        self.assertEqual(bad, after)

    def test_a_pair_edited_inside_and_moved_down_is_not_new(self) -> None:
        before = [(0.9, (self.A, 1, 7), (self.B, 10, 16))]
        after = [(0.88, (self.A, 1, 7), (self.B, 12, 18))]
        moved = {self.B: [(0, 0, 1, 2), (12, 1, 14, 1)]}
        changed = {self.B: [(1, 2), (14, 14)]}
        self.assertEqual(gate.new_pairs(before, after, changed, lambda path: moved.get(path, [])), [])

    def test_growing_a_side_past_the_old_one_fails(self) -> None:
        before = [(0.9, (self.A, 1, 7), (self.B, 10, 16))]
        after = [(0.9, (self.A, 1, 7), (self.B, 10, 26))]
        moved = {self.B: [(16, 0, 17, 10)]}
        self.assertEqual(gate.new_pairs(before, after, {self.B: [(17, 26)]}, lambda path: moved.get(path, [])), after)

    def test_a_hunk_header_without_counts_means_one_line(self) -> None:
        self.assertEqual(gate.hunks("@@ -3 +4 @@\n-a\n+b\n@@ -9,0 +10,2 @@\n"), [(3, 1, 4, 1), (9, 0, 10, 2)])

    def test_an_old_line_at_the_edges_of_a_hunk(self) -> None:
        moved = [(5, 2, 5, 3), (10, 0, 12, 2)]
        self.assertEqual(gate.new_line(moved, 4), 4)
        self.assertIsNone(gate.new_line(moved, 5))
        self.assertIsNone(gate.new_line(moved, 6))
        self.assertEqual(gate.new_line(moved, 7), 8)
        self.assertEqual(gate.new_line(moved, 10), 11)
        self.assertEqual(gate.new_line(moved, 11), 14)

    def test_a_changed_line_replaces_an_old_one_only_one_for_one(self) -> None:
        moved = [(10, 2, 12, 3)]
        self.assertIsNone(gate.replaced(moved, 11))
        self.assertEqual(gate.replaced(moved, 12), 10)
        self.assertEqual(gate.replaced(moved, 13), 11)
        self.assertIsNone(gate.replaced(moved, 14))

    def test_held_gives_where_the_head_side_sits_in_the_base_side(self) -> None:
        was = (self.A, 11, 20)
        self.assertEqual(gate.held(was, (self.A, 11, 20), {}, self.no_moves), (0.0, 1.0))
        self.assertEqual(gate.held(was, (self.A, 13, 17), {}, self.no_moves), (0.2, 0.7))
        self.assertIsNone(gate.held(was, (self.A, 21, 25), {}, self.no_moves))
        self.assertIsNone(gate.held(was, (self.B, 11, 20), {}, self.no_moves))

    def test_held_rejects_a_changed_line_that_replaced_a_line_outside_the_side(self) -> None:
        was = (self.A, 11, 20)
        self.assertIsNone(gate.held(was, (self.A, 11, 21), {self.A: [(21, 21)]}, lambda _p: [(21, 1, 21, 1)]))
        self.assertIsNone(gate.held(was, (self.A, 10, 20), {self.A: [(10, 10)]}, lambda _p: [(10, 1, 10, 1)]))

    def test_sides_line_up_only_when_their_stretches_overlap(self) -> None:
        self.assertTrue(gate.lined_up((0.0, 0.6), (0.5, 1.0)))
        self.assertFalse(gate.lined_up((0.0, 0.5), (0.5, 1.0)))
        self.assertFalse(gate.lined_up(None, (0.0, 1.0)))


class Workers(unittest.TestCase):
    def quota(self, text: str | None) -> int:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory, "cpu.max")
            if text is not None:
                path.write_text(text)
            return gate.cpu_workers(path, cores=8)

    def test_the_cgroup_quota_limits_the_workers(self) -> None:
        self.assertEqual(self.quota("200000 100000\n"), 2)
        self.assertEqual(self.quota("150000 100000\n"), 1)

    def test_no_quota_uses_the_cores(self) -> None:
        self.assertEqual(self.quota("max 100000\n"), 8)
        self.assertEqual(self.quota(None), 8)
        self.assertEqual(self.quota("garbage"), 8)

    def test_a_quota_above_the_cores_uses_the_cores(self) -> None:
        self.assertEqual(self.quota("1600000 100000\n"), 8)


class Budgeting(unittest.TestCase):
    def test_rounds_follow_the_workers_and_one_baseline_per_file(self) -> None:
        budget = gate.mutation_budget(10.0, {"a.go": [1, 2, 3, 4, 5], "b.go": [1], "c.go": []}, 60.0, workers=2)
        # a.go: ceil(5/2) + 1 = 4, b.go: 1 + 1 = 2 -> 6 rounds x 60 s + 10 s
        self.assertEqual(budget.seconds, 370.0)
        self.assertIn("6 round(s) x 60s", budget.inputs)
        self.assertFalse(budget.capped)

    def test_the_budget_is_held_to_the_floor_and_the_cap(self) -> None:
        self.assertEqual(gate.mutation_budget(0.0, {"a.go": [1]}, 1.0, workers=1).seconds, gate.MUTATION_FLOOR_SECONDS)
        big = gate.mutation_budget(0.0, {"a.go": list(range(100))}, 60.0, workers=1)
        self.assertEqual(big.seconds, gate.MUTATION_CAP_SECONDS)
        self.assertTrue(big.capped)


class Mutation(InRepo):
    """mutator is replaced by small scripts on the PATH, so these tests run no Go build."""

    def fake_mutator(self, body: str) -> None:
        bin_dir = self.repo / "bin"
        bin_dir.mkdir(exist_ok=True)
        script = bin_dir / "mutator"
        script.write_text("#!/bin/sh\n" + body)
        script.chmod(script.stat().st_mode | stat.S_IEXEC)
        patcher = mock.patch.dict(os.environ, {"PATH": f"{bin_dir}{os.pathsep}{os.environ['PATH']}"})
        patcher.start()
        self.addCleanup(patcher.stop)

    SCAN1 = f'*--scan*) echo "Scan: 1 mutation sites in {LIB}"; echo "  {LIB}:2 + -> -";;'

    def test_running_out_of_budget_fails_with_what_was_not_checked(self) -> None:
        self.fake_mutator(f'case "$*" in {self.SCAN1} *) sleep 30;; esac\n')
        started = time.monotonic()
        ok, lines = gate.check_mutation({LIB: [(2, 2)]}, self.repo, {}, size=fixed(1.0))
        self.assertLess(time.monotonic() - started, 15)
        self.assertFalse(ok)
        text = "\n".join(lines)
        self.assertIn("budget ran out", text)
        self.assertIn(f"{LIB} (1 mutation site(s))", text)

    def test_a_finished_run_reports_survivors_and_their_waivers(self) -> None:
        self.fake_mutator(
            f'case "$*" in *--scan*) echo "Scan: 2 mutation sites in {LIB}"; echo "  {LIB}:2 + -> -"; echo "* {LIB}:3 == -> !=";;\n'
            f'*) echo "KILLED {LIB}:2 + -> -"; echo "SURVIVED {LIB}:3 == -> !="; exit 3;; esac\n'
        )
        ok, lines = gate.check_mutation({LIB: [(2, 3)]}, self.repo, {}, size=fixed(30.0))
        self.assertFalse(ok)
        self.assertIn("1 mutant(s) killed, 1 survived", lines[0])
        ok, lines = gate.check_mutation({LIB: [(2, 3)]}, self.repo, {(LIB, 3): "the comparison is symmetric here"}, size=fixed(30.0))
        self.assertTrue(ok, lines)
        self.assertIn("explained as equivalent", "\n".join(lines))

    def test_the_file_reaches_the_test_command_through_the_environment_only(self) -> None:
        self.fake_mutator(f'case "$*" in {self.SCAN1}\n*) echo "$*" > args.txt; echo "$MUTANT_FILE" > file.txt; exit 0;; esac\n')
        gate.check_mutation({LIB: [(2, 2)]}, self.repo, {}, size=fixed(30.0))
        self.assertEqual((self.repo / "file.txt").read_text().strip(), LIB)
        args = (self.repo / "args.txt").read_text()
        self.assertIn(f"--test-command ./{gate.MUTANT_TESTS}", args)
        self.assertIn("--max-workers", args)
        self.assertEqual(args.count(LIB), 1)

    def test_a_mutator_error_fails_the_check(self) -> None:
        self.fake_mutator(f'case "$*" in {self.SCAN1} *) echo boom >&2; exit 1;; esac\n')
        ok, lines = gate.check_mutation({LIB: [(2, 2)]}, self.repo, {}, size=fixed(30.0))
        self.assertFalse(ok)
        self.assertIn("mutator failed", lines[0])
        self.assertIn("boom", lines[0])

    def test_a_scan_that_crashes_or_skips_the_file_fails_the_check(self) -> None:
        for body in ('echo "Traceback" >&2; exit 1', 'echo "No source files to mutate."', f'echo "Scan: 2 mutation sites in {LIB}"'):
            with self.subTest(body=body):
                self.fake_mutator(f'case "$*" in *--scan*) {body};; *) exit 0;; esac\n')
                ok, lines = gate.check_mutation({LIB: [(2, 2)]}, self.repo, {}, size=fixed(30.0))
                self.assertFalse(ok)
                self.assertIn("mutator --scan failed", lines[0])

    def test_a_scan_with_no_sites_passes_without_mutating(self) -> None:
        self.fake_mutator(f'case "$*" in *--scan*) echo "Scan: 0 mutation sites in {LIB}";; *) touch ran; exit 1;; esac\n')
        ok, lines = gate.check_mutation({LIB: [(2, 2)]}, self.repo, {}, size=fixed(30.0))
        self.assertTrue(ok, lines)
        self.assertFalse((self.repo / "ran").exists())

    def test_a_site_with_no_verdict_fails_unless_waived(self) -> None:
        self.fake_mutator(
            f'case "$*" in *--scan*) echo "Scan: 2 mutation sites in {LIB}"; echo "  {LIB}:2 + -> -"; echo "  {LIB}:3 == -> !=";;\n'
            f'*) echo "KILLED {LIB}:2 + -> -"; echo "UNCOVERED {LIB}:3 == -> !="; exit 0;; esac\n'
        )
        ok, lines = gate.check_mutation({LIB: [(2, 3)]}, self.repo, {}, size=fixed(30.0))
        self.assertFalse(ok)
        self.assertIn(f"  no result {LIB}:3 (NOT explained)", "\n".join(lines))
        ok, lines = gate.check_mutation({LIB: [(2, 3)]}, self.repo, {(LIB, 3): "only reached on a real signal"}, size=fixed(30.0))
        self.assertTrue(ok, lines)

    def test_results_logged_before_the_budget_ran_out_are_kept(self) -> None:
        self.fake_mutator(
            f'case "$*" in *--scan*) echo "Scan: 2 mutation sites in {LIB}"; echo "  {LIB}:2 + -> -"; echo "  {LIB}:3 == -> !=";;\n'
            '*) printf "2\\tkilled\\n3\\tsurvived\\n" >> "$QUALITY_MUTANT_LOG"; sleep 30;; esac\n'
        )
        ok, lines = gate.check_mutation({LIB: [(2, 3)]}, self.repo, {}, size=fixed(1.0))
        self.assertFalse(ok)
        self.assertTrue(lines[0].startswith("1 mutant(s) killed, 1 survived"), lines[0])


@unittest.skipUnless(shutil.which("go"), "go is not installed")
class MutantTests(InRepo):
    """mutant-tests.sh runs the mutated package's tests in mutator's worker copy and logs the verdict by line."""

    def setUp(self) -> None:
        super().setUp()
        self.write(LIB, "package core\n\nfunc Inc(a int) int {\n\treturn a + 1\n}\n")
        self.write("internal/core/core_test.go", "package core\n\nimport \"testing\"\n\nfunc TestInc(t *testing.T) {\n\tif Inc(1) != 2 {\n\t\tt.Fatal()\n\t}\n}\n")
        shutil.copy(HERE / "mutant-tests.sh", self.repo / "mutant-tests.sh")
        self.worker = self.repo / "worker"
        shutil.copytree(self.repo, self.worker, ignore=shutil.ignore_patterns("worker", ".git"))
        self.log = self.repo / "verdicts.log"

    def run_mutant(self, source: str) -> int:
        (self.worker / LIB).write_text(source)
        env = {**os.environ, "MUTANT_FILE": LIB, "QUALITY_ROOT": str(self.repo), "QUALITY_MUTANT_LOG": str(self.log)}
        return subprocess.run(["sh", str(self.repo / "mutant-tests.sh")], cwd=self.worker, env=env, capture_output=True).returncode

    def test_a_killed_mutant_is_logged_at_its_line(self) -> None:
        self.assertNotEqual(self.run_mutant("package core\n\nfunc Inc(a int) int {\n\treturn a - 1\n}\n"), 0)
        self.assertEqual(self.log.read_text(), "4\tkilled\n")

    def test_a_surviving_mutant_is_logged_as_survived(self) -> None:
        self.assertEqual(self.run_mutant("package core\n\nfunc Inc(a int) int {\n\treturn 1 + a\n}\n"), 0)
        self.assertEqual(self.log.read_text(), "4\tsurvived\n")

    def test_the_unchanged_file_logs_nothing(self) -> None:
        self.assertEqual(self.run_mutant((self.repo / LIB).read_text()), 0)
        self.assertFalse(self.log.exists())


if __name__ == "__main__":
    unittest.main()
