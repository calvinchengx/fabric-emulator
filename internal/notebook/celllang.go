package notebook

// What a cell's language means for execution.
//
// THE PARSER ALREADY READ THE MAGIC; the driver used to ignore the answer.
// `detectLanguage` recognises `%%configure`, `%%spark`, `%%html` and the rest,
// and the run loop then sent everything that was not `sql` to the Python
// executor. So a Scala cell did not fail with "Scala is not supported here" —
// it failed with a Python SyntaxError, pointing at the user's own code, and a
// `%%configure` block of JSON failed the same way.
//
// That is the plausible-wrong-answer shape this repo exists to refuse. A cell
// in a language the emulator cannot run must say WHICH language and that it
// cannot run it; a cell that is not code at all must not be executed; and a
// directive the emulator honours by doing nothing must say it did nothing,
// rather than let the author believe eight executors arrived.

import "strings"

// CellDisposition is what the run loop should do with a cell.
type CellDisposition int

const (
	// RunPython — the default, and `%%pyspark`.
	RunPython CellDisposition = iota
	// RunSQL — `%%sql`.
	RunSQL
	// Render — not code. `%%html`, `%%markdown`: content to display.
	Render
	// Ignored — a directive this emulator honours by doing nothing, and says so.
	Ignored
	// Unsupported — a real language this emulator cannot execute.
	Unsupported
)

// Disposition maps a cell language to what should happen to it, plus the note
// a human needs when that is anything other than "run it".
func Disposition(language string) (CellDisposition, string) {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "", "python", "pyspark":
		return RunPython, ""
	case "sql", "spark.sql":
		return RunSQL, ""
	case "html", "markdown", "md":
		// Content, not code. Executing it as Python is how a `%%html` cell
		// used to fail with a SyntaxError about its own markup.
		return Render, "rendered, not executed"
	case "configure":
		// ACCEPTED AND IGNORED, deliberately, and never silently. `%%configure`
		// asks for session resources — executors, memory, conf. This emulator
		// has one session and nothing to size, so there is nothing to switch
		// and honouring the request by doing nothing is correct emulation (the
		// same rule contract 2 states for an accepted-and-ignored parameter).
		//
		// Refusing instead would be worse: `%%configure` must be the first
		// cell on Fabric, so a refusal makes every notebook that carries one
		// unrunnable here, and the results it would have produced are correct
		// — just not on the requested hardware.
		return Ignored, "%%configure is accepted and IGNORED: this emulator " +
			"has one session and no resources to size, so the cell changed " +
			"nothing. Results are unaffected; the requested executors, memory " +
			"and conf were not applied"
	case "scala", "spark", "sparkr", "r", "csharp":
		// A REAL LANGUAGE THIS CANNOT RUN. Named, because the alternative was
		// a Python SyntaxError pointing at correct Scala.
		return Unsupported, "this cell is " + language + ", which this emulator " +
			"cannot execute: the agent runs Python and SQL. Real Fabric runs " +
			"it. Port the cell to %%pyspark, or run it against a real workspace"
	}
	return RunPython, ""
}
