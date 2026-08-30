"""The HTML extraction behind the vendored Fabric activity table.

The checker holds the Go list to the vendored file, and the vendored file is
whatever `extract()` pulled out of Microsoft's page. So `extract()` is the load-
bearing part of that chain and it shipped with no test at all — a parse that
quietly returns the wrong rows would vendor a table the checker then agrees
with, and agreement with a bad oracle reads exactly like correctness.

The network fetch is not tested and does not need to be: it is one urlopen. The
parsing, and its two refusals, are.
"""
import json
import pathlib
import sys

import pytest

ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts"))

import vendor_fabric_activity_types as v  # noqa: E402


def page(rows: str, heading: str | None = None) -> str:
    h = heading or v.SECTION
    return (f"<h3 id=x>{h}</h3>\n<table>\n<thead><tr><th>Name</th><th>Description</th></tr>"
            f"</thead>\n<tbody>\n{rows}\n</tbody>\n</table>\n")


def row(name: str, desc: str, link: bool = True) -> str:
    cell = f'<a href="#x" data-linktype="self-bookmark">{name}</a>' if link else name
    return f"<tr>\n<td>{cell}</td>\n<td>{desc}</td>\n</tr>"


def enough(extra: str = "") -> str:
    return "\n".join([row(f"Type{i}", f"does thing {i}") for i in range(25)]) + extra


def test_it_reads_names_and_descriptions():
    got = v.extract(page(enough()))
    assert got["Type0"] == "does thing 0"
    assert len(got) == 25


def test_markup_and_entities_are_stripped():
    """The name is a link and the description carries entities; the vendored
    artifact must hold neither."""
    got = v.extract(page(enough(row("Web&amp;Hook", "Calls a <em>webhook</em> &amp; waits"))))
    assert "Web&Hook" in got
    assert got["Web&Hook"] == "Calls a webhook & waits"


def test_whitespace_in_a_description_is_collapsed():
    got = v.extract(page(enough(row("Spaced", "one\n   two\t three"))))
    assert got["Spaced"] == "one two three"


def test_the_result_is_sorted():
    """A stable order is what makes the sha256 pin meaningful — an unsorted dict
    would re-hash on every fetch and every refresh would look like a change."""
    got = v.extract(page(enough(row("Aaa", "first")) + row("Zzz", "last")))
    assert list(got) == sorted(got)


def test_a_missing_heading_refuses():
    with pytest.raises(SystemExit, match="heading not found"):
        v.extract(page(enough(), heading="SomeOtherSection"))


def test_a_heading_with_no_table_refuses():
    with pytest.raises(SystemExit, match="no table"):
        v.extract(f"<h3>{v.SECTION}</h3><p>prose, no table</p>")


def test_a_partial_parse_refuses_rather_than_vendoring_it():
    """THE ONE THAT MATTERS. A parse that finds three rows would vendor a table
    the checker then 'agrees' with, silently narrowing the guard to nothing."""
    with pytest.raises(SystemExit, match="refusing to vendor a partial table"):
        v.extract(page(row("Copy", "copies") + row("Wait", "waits")))


def test_it_anchors_on_the_heading_not_the_first_table():
    """The article carries several tables; position is not a contract."""
    other = "<table><tbody>" + row("Decoy", "a table earlier on the page") + "</tbody></table>"
    got = v.extract("<h2>Something else</h2>" + other + page(enough()))
    assert "Decoy" not in got


def test_render_is_deterministic_and_carries_its_source():
    rows = v.extract(page(enough()))
    a, b = v.render(rows), v.render(rows)
    assert a == b, "the artifact must be byte-stable or its sha256 pin is noise"
    doc = json.loads(a)
    assert doc["source"] == v.URL and doc["section"] == v.SECTION
    assert doc["types"] == rows
