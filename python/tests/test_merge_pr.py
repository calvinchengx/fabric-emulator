"""Controls for scripts/merge_pr.py's message building.

The merge itself needs GitHub, so what is pinned here is the part that decides
what lands: the subject gets the ` (#N)` suffix exactly once, and no trailer
line in the description survives into the body.

Run:  uv run --no-project --python 3.12 python -m unittest python/tests/test_merge_pr.py
"""
import pathlib
import sys
import unittest
from unittest import mock

HERE = pathlib.Path(__file__).resolve().parents[2] / "scripts"
sys.path.insert(0, str(HERE))
import merge_pr as merge_pr  # noqa: E402


class BuildMessage(unittest.TestCase):
    def test_subject_gets_the_number_once(self):
        self.assertEqual(merge_pr.build_message("fix: x", 7, "")[0], "fix: x (#7)")
        self.assertEqual(merge_pr.build_message("fix: x (#7)", 7, "")[0], "fix: x (#7)")

    def test_trailers_in_the_description_are_dropped(self):
        body = ("Bumps a thing.\n\nSigned-off-by: dependabot[bot] <support@github.com>\n"
                "Co-authored-by: dependabot[bot] <bot@users.noreply.github.com>\n"
                "Claude-Session: https://claude.ai/code/session_x\n")
        _, out = merge_pr.build_message("t", 1, body)
        self.assertEqual(merge_pr.offending_lines(out), [])
        self.assertIn("Signed-off-by: dependabot[bot]", out)
        self.assertTrue(out.startswith("Bumps a thing."))

    def test_empty_description(self):
        self.assertEqual(merge_pr.build_message("t", 1, None)[1], "\n")


class Merge(unittest.TestCase):
    """main() against a scripted `gh`: what it refuses, what it sends, and what
    it does when GitHub lands trailers anyway. Nothing here reaches GitHub."""

    def run_main(self, state="OPEN", landed="fix: x (#7)\n\nBody\n", body="Body\n"):
        import contextlib
        import io
        import json
        calls = []

        def fake(argv, **kw):
            calls.append((argv, kw.get("input")))
            out = ""
            if argv[:3] == ["gh", "pr", "view"] and "title,body,state,headRefOid" in argv:
                out = json.dumps({"title": "fix: x", "body": body, "state": state, "headRefOid": "h" * 40})
            elif argv[:3] == ["gh", "pr", "view"] and "mergeCommit" in argv:
                out = json.dumps({"mergeCommit": {"oid": "m" * 40}})
            elif argv[:2] == ["gh", "api"]:
                out = json.dumps({"commit": {"message": landed}})
            return type("R", (), {"stdout": out, "returncode": 0})()

        err, out = io.StringIO(), io.StringIO()
        with mock.patch.object(merge_pr.subprocess, "run", fake), \
                contextlib.redirect_stderr(err), contextlib.redirect_stdout(out):
            status = merge_pr.main(["7"])
        return status, calls, out.getvalue(), err.getvalue()

    def test_a_pull_request_that_is_not_open_is_refused_before_any_merge(self):
        status, calls, _, err = self.run_main(state="MERGED")
        self.assertEqual(status, 1)
        self.assertIn("is merged, not open", err)
        self.assertFalse(any(a[:3] == ["gh", "pr", "merge"] for a, _ in calls))

    def test_a_clean_merge_sends_our_message_pinned_to_the_head_it_read(self):
        status, calls, out, _ = self.run_main(body="Body\n\nCo-authored-by: x <x@x>\n")
        self.assertEqual(status, 0)
        [(argv, sent)] = [(a, i) for a, i in calls if a[:3] == ["gh", "pr", "merge"]]
        self.assertEqual(argv[3:], ["7", "--squash", "--delete-branch", "--match-head-commit", "h" * 40,
                                    "--subject", "fix: x (#7)", "--body-file", "-"])
        self.assertNotIn("Co-authored-by", sent)
        self.assertIn("landed as mmmmmmmmmmmm with no co-authorship trailers", out)

    def test_trailers_github_adds_anyway_fail_the_merge_loudly(self):
        """The read-back is the point: the message we sent being clean says
        nothing about what GitHub wrote. The fixture is a PERSON, because a
        dependency bot's trailer is allowed now -- see the test below."""
        status, _, _, err = self.run_main(landed="fix: x (#7)\n\nCo-authored-by: Someone <s@s>\n")
        self.assertEqual(status, 1)
        self.assertIn("landed WITH trailers despite the explicit message", err)
        self.assertIn("Co-authored-by: Someone", err)

    def test_a_dependency_bot_trailer_is_not_a_failure(self):
        """GitHub writes one for every Dependabot squash merge made by someone
        else, and a dependency service is not claiming authorship."""
        status, _, out, _ = self.run_main(landed="fix: x (#7)\n\nCo-authored-by: dependabot[bot] <b@b>\n")
        self.assertEqual(status, 0)
        self.assertIn("landed as mmmmmmmmmmmm with no co-authorship trailers", out)


if __name__ == "__main__":
    unittest.main()
