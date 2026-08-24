package notebook_test

// A cell's language decides what happens to it, and getting this wrong was not
// a missing feature — it was a WRONG ANSWER. The parser recognised
// `%%configure`, `%%spark` and `%%html`, and the run loop then sent everything
// that was not `sql` to the Python executor. Correct Scala came back as a
// Python SyntaxError pointing at the user's own code.

import (
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/notebook"
)

func TestPythonAndItsSpellingsRun(t *testing.T) {
	for _, lang := range []string{"", "python", "pyspark", "PySpark", "  python  "} {
		d, note := notebook.Disposition(lang)
		if d != notebook.RunPython {
			t.Fatalf("%q: disposition = %v, want RunPython", lang, d)
		}
		if note != "" {
			t.Fatalf("%q: unexpected note %q", lang, note)
		}
	}
}

func TestSQLRunsAsSQL(t *testing.T) {
	for _, lang := range []string{"sql", "SQL", "spark.sql"} {
		if d, _ := notebook.Disposition(lang); d != notebook.RunSQL {
			t.Fatalf("%q: disposition = %v, want RunSQL", lang, d)
		}
	}
}

// CONTENT IS NOT CODE. `%%html` used to be executed as Python, so a cell of
// markup failed with a SyntaxError about its own tags.
func TestMarkupIsRenderedNotExecuted(t *testing.T) {
	for _, lang := range []string{"html", "markdown", "md"} {
		d, note := notebook.Disposition(lang)
		if d != notebook.Render {
			t.Fatalf("%q: disposition = %v, want Render", lang, d)
		}
		if note == "" {
			t.Fatalf("%q: a cell that did not execute must say so", lang)
		}
	}
}

// ACCEPTED AND IGNORED, NEVER SILENTLY. Refusing would be worse — `%%configure`
// must be the first cell on Fabric, so a refusal makes every notebook carrying
// one unrunnable here, and the results it would produce are correct, just not
// on the requested hardware.
func TestConfigureIsIgnoredOutLoud(t *testing.T) {
	d, note := notebook.Disposition("configure")
	if d != notebook.Ignored {
		t.Fatalf("disposition = %v, want Ignored", d)
	}
	if !strings.Contains(note, "IGNORED") {
		t.Fatalf("the note must say it was ignored, got %q", note)
	}
	if !strings.Contains(note, "Results are unaffected") {
		t.Fatalf("the note must say what was and was not affected, got %q", note)
	}
}

// A REAL LANGUAGE THIS CANNOT RUN gets named. The alternative shipped for
// months: a Python SyntaxError pointing at correct Scala.
func TestUnsupportedLanguagesAreNamedNotMisExecuted(t *testing.T) {
	for _, lang := range []string{"scala", "spark", "sparkr", "r", "csharp"} {
		d, note := notebook.Disposition(lang)
		if d != notebook.Unsupported {
			t.Fatalf("%q: disposition = %v, want Unsupported", lang, d)
		}
		if !strings.Contains(note, lang) {
			t.Fatalf("%q: the error must name the language, got %q", lang, note)
		}
		if !strings.Contains(note, "Real Fabric runs it") {
			t.Fatalf("%q: the error must not read as 'this is invalid', got %q", lang, note)
		}
	}
}

// `%%spark` is Scala on Fabric, and the parser already normalises it. If that
// mapping were lost, a Scala cell would run as Python again.
func TestSparkMagicIsScalaNotPython(t *testing.T) {
	if d, _ := notebook.Disposition("spark"); d != notebook.Unsupported {
		t.Fatalf("%%spark must not be treated as Python: got %v", d)
	}
}

// An unknown magic is NOT refused. Fabric adds magics; refusing every one this
// build has not heard of would break notebooks on upgrade, and Python is the
// documented default for a cell with no language.
func TestAnUnknownLanguageFallsBackToPython(t *testing.T) {
	d, note := notebook.Disposition("someFutureMagic")
	if d != notebook.RunPython {
		t.Fatalf("disposition = %v, want RunPython", d)
	}
	if note != "" {
		t.Fatalf("unexpected note %q", note)
	}
}
