"""Controls for scripts/check_attribution.py.

A gate that never fires looks exactly like a clean tree, so each trailer it
exists to stop is fed in and required to fail, and the near-misses it must let
through are fed in and required to pass. The last test runs it against a real
throwaway git repository, so the `git log` parsing is exercised too.

Run:  uv run --no-project --python 3.12 python -m unittest python/tests/test_check_attribution.py
"""
import contextlib
import io
import pathlib
import subprocess
import sys
import tempfile
import unittest

HERE = pathlib.Path(__file__).resolve().parents[2] / "scripts"
sys.path.insert(0, str(HERE))
import check_attribution as attribution  # noqa: E402


class TrailerMatching(unittest.TestCase):
    def test_rejects_each_trailer_in_any_case(self):
        for line in (
            "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>",
            "co-authored-by: Someone <a@b.c>",
            "Claude-Session: https://claude.ai/code/session_x",
            "  Co-authored-by : indented <a@b.c>",
        ):
            with self.subTest(line=line):
                self.assertEqual(attribution.offending_lines(f"fix: x\n\nbody\n\n{line}\n"), [line.strip()])

    def test_allows_mentions_that_are_not_trailers(self):
        text = "docs: explain why we reject `Co-authored-by:` trailers\n\nNo Claude-Session: here either."
        self.assertEqual(attribution.offending_lines(text), [])

    def test_reports_commits_and_description_separately(self):
        findings = attribution.check(
            [("a" * 40, "feat: x\n\nCo-Authored-By: Bot <b@b>"), ("b" * 40, "fix: clean")],
            "Summary\n\nClaude-Session: https://claude.ai/code/session_y",
        )
        self.assertEqual(findings, [
            "commit aaaaaaaaaaaa: Co-Authored-By: Bot <b@b>",
            "pull request description: Claude-Session: https://claude.ai/code/session_y",
        ])

    def test_clean_input_has_no_findings(self):
        self.assertEqual(attribution.check([("c" * 40, "fix: clean\n\nSigned-off-by: A <a@b>")], "Body"), [])


class AgainstGit(unittest.TestCase):
    def git(self, repo, *args):
        return subprocess.run(["git", "-C", repo, *args], check=True, capture_output=True, text=True).stdout.strip()

    def test_range_is_read_from_git(self):
        with tempfile.TemporaryDirectory() as repo:
            self.git(repo, "init", "-q")
            self.git(repo, "config", "user.email", "t@example.com")
            self.git(repo, "config", "user.name", "T")
            self.git(repo, "commit", "-q", "--allow-empty", "-m", "base")
            base = self.git(repo, "rev-parse", "HEAD")
            self.git(repo, "commit", "-q", "--allow-empty", "-m", "clean change")
            self.git(repo, "commit", "-q", "--allow-empty", "-m",
                     "tainted change\n\nCo-Authored-By: Claude <noreply@anthropic.com>")
            tainted = self.git(repo, "rev-parse", "HEAD")

            out = io.StringIO()
            with contextlib.chdir(repo), contextlib.redirect_stdout(out):
                status = attribution.main(["--range", f"{base}..HEAD"])
            self.assertEqual(status, 1, out.getvalue())
            self.assertIn(f"commit {tainted[:12]}: Co-Authored-By: Claude", out.getvalue())

            with contextlib.chdir(repo), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(attribution.main(["--range", f"{base}..HEAD~1"]), 0)


    def test_the_description_file_is_read_and_a_clean_run_says_so(self):
        """The pull request run passes the description through a file, because
        it is attacker-controlled on a fork. Both outcomes of that path."""
        with tempfile.TemporaryDirectory() as repo:
            self.git(repo, "init", "-q")
            self.git(repo, "config", "user.email", "t@example.com")
            self.git(repo, "config", "user.name", "T")
            self.git(repo, "commit", "-q", "--allow-empty", "-m", "base")
            self.git(repo, "commit", "-q", "--allow-empty", "-m", "clean change")
            body = pathlib.Path(repo) / "body.md"

            body.write_text("Summary\n\nCo-authored-by: Someone <a@b.c>\n", encoding="utf-8")
            out = io.StringIO()
            with contextlib.chdir(repo), contextlib.redirect_stdout(out):
                status = attribution.main(["--range", "HEAD~1..HEAD", "--body-file", str(body)])
            self.assertEqual(status, 1, out.getvalue())
            self.assertIn("pull request description: Co-authored-by: Someone", out.getvalue())

            body.write_text("Summary only.\n", encoding="utf-8")
            out = io.StringIO()
            with contextlib.chdir(repo), contextlib.redirect_stdout(out):
                status = attribution.main(["--range", "HEAD~1..HEAD", "--body-file", str(body)])
            self.assertEqual(status, 0, out.getvalue())
            self.assertIn("1 commit(s) and the description carry no co-authorship trailers", out.getvalue())


if __name__ == "__main__":
    unittest.main()
