"""Unit tests for the half of the Desktop probe that does not need Desktop.

These run anywhere, in milliseconds. What they deliberately do NOT test is
whether Power BI Desktop installs, launches or listens — that is the finding
the suite exists to produce, and a test that faked it would be reporting an
absence as a presence.
"""

import math
import pathlib

import pytest

from verify import ProbeError, compare, parse_port_file, parse_rows, stage_results


class TestParsePortFile:
    # Desktop writes UTF-16 with a BOM. Read as UTF-8 it becomes a string full
    # of NULs, and int() then fails with a message naming neither the encoding
    # nor the file.
    def test_reads_the_utf16_desktop_actually_writes(self):
        assert parse_port_file("59321".encode("utf-16")) == 59321

    def test_reads_utf8_and_bom_variants(self):
        assert parse_port_file(b"59321") == 59321
        assert parse_port_file(b"\xef\xbb\xbf59321") == 59321
        assert parse_port_file("59321\r\n".encode("utf-16-le")) == 59321

    def test_an_empty_file_is_not_a_port(self):
        # Desktop creates the file before it writes to it, so an empty read is
        # "too early", not "no Analysis Services". The caller must retry, which
        # it cannot decide to do if this returned 0.
        with pytest.raises(ProbeError, match="empty"):
            parse_port_file(b"")

    def test_a_zero_or_out_of_range_port_is_rejected(self):
        for bad in (b"0", b"70000"):
            with pytest.raises(ProbeError, match="range"):
                parse_port_file(bad)

    def test_garbage_says_what_it_saw(self):
        with pytest.raises(ProbeError, match="could not read a port"):
            parse_port_file(b"<html>404</html>")


class TestParseRows:
    SAMPLE = """PLATFORM Win32NT/X64
STAGE connect :: OK
ROW Customer[Country]=GB [Revenue]=25227674.7 [PerUnit]=101.72941714921689
ROW Customer[Country]=SG [Revenue]=24819216.2 [PerUnit]=100.46638681994818
STAGE query :: OK
"""

    def test_reads_only_row_lines(self):
        rows = parse_rows(self.SAMPLE)
        assert len(rows) == 2
        assert rows[0]["Customer[Country]"] == "GB"
        assert rows[0]["[Revenue]"] == "25227674.7"

    def test_a_failed_connection_yields_no_rows_not_an_empty_model(self):
        # The distinction this test protects: a parser that scraped every line
        # would turn a connection failure into an empty result set, which reads
        # as "the model has no rows" rather than "nothing connected".
        failed = "STAGE connect :: AdomdConnectionException :: refused\n"
        assert parse_rows(failed) == []
        assert stage_results(failed)["connect"].startswith("AdomdConnectionException")

    def test_empty_values_survive(self):
        rows = parse_rows("ROW Customer[Country]= [Revenue]=0\n")
        assert rows[0]["Customer[Country]"] == ""


class TestStageResults:
    def test_separates_the_stages(self):
        # "installed but never listened" and "listened but the query failed"
        # are different findings; one pass/fail would hide which happened.
        out = stage_results("STAGE connect :: OK\nSTAGE query :: AdomdErrorResponseException :: bad DAX\n")
        assert out["connect"] == "OK"
        assert out["query"].startswith("AdomdErrorResponseException")

    def test_absent_stage_is_absent_not_ok(self):
        out = stage_results("STAGE connect :: OK\n")
        assert "query" not in out


EXPECTED = {
    "GB": {"Total Revenue": 25227674.70, "Revenue per Unit": 101.72941714921689},
    "SG": {"Total Revenue": 24819216.20, "Revenue per Unit": 100.46638681994818},
}
FIELDS = {"Total Revenue": "[Revenue]", "Revenue per Unit": "[PerUnit]"}


def rows_for(gb_rev, gb_per, sg_rev=24819216.2, sg_per=100.46638681994818):
    return [
        {"Customer[Country]": "GB", "[Revenue]": str(gb_rev), "[PerUnit]": str(gb_per)},
        {"Customer[Country]": "SG", "[Revenue]": str(sg_rev), "[PerUnit]": str(sg_per)},
    ]


class TestCompare:
    def test_agrees_on_identical_numbers(self):
        ok, _ = compare(EXPECTED, rows_for(25227674.70, 101.72941714921689),
                        "Customer[Country]", FIELDS)
        assert ok

    def test_tolerates_last_bit_float_noise(self):
        # IEEE rounding, not disagreement: two engines summing the same doubles
        # in a different order land one ulp apart.
        ok, _ = compare(EXPECTED, rows_for(25227674.70, 101.72941714921690),
                        "Customer[Country]", FIELDS)
        assert ok

    def test_the_tolerance_is_relative_and_this_is_the_case_that_proves_it(self):
        """One ulp on a 25-million figure is 3.7e-09 ABSOLUTE.

        That is the number Phase 0 actually hit, and it is why `compare` scales
        by magnitude. An absolute 1e-9 epsilon rejects it as a divergence — so
        without this case, swapping the relative tolerance for an absolute one
        passes every other test in this file. It was caught by mutating the
        implementation and finding the suite silent.
        """
        one_ulp_up = math.nextafter(25227674.70, math.inf)
        assert abs(one_ulp_up - 25227674.70) >= 1e-9, "the premise: absolute epsilon would reject this"
        ok, _ = compare(EXPECTED, rows_for(one_ulp_up, 101.72941714921689),
                        "Customer[Country]", FIELDS)
        assert ok

    def test_catches_a_real_divergence_an_absolute_epsilon_would_miss(self):
        # A penny out on a per-unit RATE is a real error, and is smaller in
        # absolute terms than the float noise tolerated on a 25-million total.
        ok, lines = compare(EXPECTED, rows_for(25227674.70, 101.73941714921689),
                            "Customer[Country]", FIELDS)
        assert not ok
        assert any("101.739" in ln for ln in lines)

    def test_a_missing_group_fails_loudly(self):
        rows = [r for r in rows_for(25227674.70, 101.72941714921689)
                if r["Customer[Country]"] != "SG"]
        ok, lines = compare(EXPECTED, rows, "Customer[Country]", FIELDS)
        assert not ok
        assert any("no row for: SG" in ln for ln in lines)

    def test_an_extra_group_fails_loudly(self):
        rows = rows_for(25227674.70, 101.72941714921689)
        rows.append({"Customer[Country]": "FR", "[Revenue]": "1", "[PerUnit]": "1"})
        ok, lines = compare(EXPECTED, rows, "Customer[Country]", FIELDS)
        assert not ok
        assert any("did not expect: FR" in ln for ln in lines)

    def test_no_rows_at_all_is_a_failure_not_a_pass(self):
        # The failure mode that matters most: Desktop connects, returns nothing,
        # and a lenient comparison calls it agreement.
        ok, lines = compare(EXPECTED, [], "Customer[Country]", FIELDS)
        assert not ok
        assert any("no row for" in ln for ln in lines)

    def test_a_non_numeric_cell_is_reported_not_crashed_on(self):
        rows = rows_for("#ERR", 101.72941714921689)
        ok, lines = compare(EXPECTED, rows, "Customer[Country]", FIELDS)
        assert not ok
        assert any("not a number" in ln for ln in lines)


class TestDesktopScriptParses:
    """desktop.ps1 only ever runs on Windows — its SYNTAX does not have to.

    This suite shipped a `param()` block below `$ErrorActionPreference`, which
    PowerShell rejects outright, and the ParserError was found by a CI run
    rather than here. pwsh parses a script on any platform, so there was never
    a reason to learn that from a runner.
    """

    def test_powershell_can_parse_it(self):
        import shutil
        import subprocess

        pwsh = shutil.which("pwsh") or shutil.which("powershell")
        if not pwsh:
            pytest.skip("no PowerShell available to parse with")
        script = pathlib.Path(__file__).resolve().parent / "desktop.ps1"
        # Parse only — never execute. Tokenize+ParseFile reports syntax errors
        # without installing a 500 MB GUI application on the contributor's
        # machine, which is the whole distinction this file is built around.
        r = subprocess.run(
            [pwsh, "-NoProfile", "-Command",
             "$e=$null; $t=$null;"
             f"[System.Management.Automation.Language.Parser]::ParseFile('{script}',[ref]$t,[ref]$e) > $null;"
             "if ($e) { $e | ForEach-Object { $_.ToString() }; exit 1 }"],
            capture_output=True, text=True)
        assert r.returncode == 0, f"desktop.ps1 does not parse:\n{r.stdout}{r.stderr}"
