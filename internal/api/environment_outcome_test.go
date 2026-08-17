package api

import (
	"strings"
	"testing"
)

// THE GAP THESE PIN. Applying an Environment used to be fire-and-forget, so a
// binding that could not be honoured — an environment that cannot be read, an
// agent that is unreachable, an agent that DECLINES because another session
// already holds a different environment — left the run finishing Completed
// with the run detail still listing the Environment. The caller then hit
// ModuleNotFoundError inside a cell and had nothing anywhere telling them the
// packages were never installed, so they debugged their own code for a
// dependency the platform had silently dropped.
//
// applyEnvironment now reports its outcome, and the two drivers that produce a
// RUN RECORD act on it. The Livy session path deliberately does not: it has no
// run detail to misreport, and the client sees the failure directly on its next
// statement. That asymmetry is asserted below so a refactor cannot quietly
// flatten it either way.

func TestUnreadableEnvironmentIsReportedNotSwallowed(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	out := a.applyEnvironment("sess-1", ws.ID, "00000000-0000-0000-0000-000000000000")

	if out.OK {
		t.Fatal("an environment that cannot be read reported OK")
	}
	if !strings.Contains(out.Reason, "cannot be read") {
		t.Fatalf("reason %q should say the environment could not be read", out.Reason)
	}
}

func TestAgentDeclinedEnvironmentIsReported(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	env := seedEnvironment(t, st, ws.ID, "team-env", "anytree\n")
	stub := newAgentStub(t, a)
	// The agent declines when another session already bound a different
	// environment: the emulator cannot isolate them per container, so refusing
	// is the honest answer — and the caller has to be told.
	stub.answer(map[string]any{
		"applied": false,
		"reason":  "session sess-9 already bound environment other-env",
	}, 0)

	out := a.applyEnvironment("sess-1", ws.ID, env.ID)

	if out.OK {
		t.Fatal("a declined environment reported OK")
	}
	if !strings.Contains(out.Reason, "was not applied") ||
		!strings.Contains(out.Reason, "already bound") {
		t.Fatalf("reason %q should carry the agent's own reason", out.Reason)
	}
}

func TestAppliedEnvironmentReportsOK(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	env := seedEnvironment(t, st, ws.ID, "team-env", "anytree\n")
	stub := newAgentStub(t, a)
	stub.answer(map[string]any{"applied": true}, 0)

	if out := a.applyEnvironment("sess-1", ws.ID, env.ID); !out.OK {
		t.Fatalf("a successfully applied environment reported %+v", out)
	}
}

// Binding nothing is honoured by having nothing to do — it must not start
// failing runs that never asked for an environment.
func TestNoEnvironmentBoundIsOK(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	if out := a.applyEnvironment("sess-1", ws.ID, ""); !out.OK {
		t.Fatalf("an empty binding reported %+v", out)
	}
}

// An environment that resolves but declares nothing is also honoured: there is
// no install to fail. Without this the emulator would fail every run binding an
// empty environment, which is a working shape today.
func TestEmptyEnvironmentIsOKAndSendsNothing(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	env := seedEnvironment(t, st, ws.ID, "bare-env", "")
	stub := newAgentStub(t, a)

	if out := a.applyEnvironment("sess-1", ws.ID, env.ID); !out.OK {
		t.Fatalf("an environment with nothing to install reported %+v", out)
	}
	if n := len(stub.recorded()); n != 0 {
		t.Fatalf("agent was called %d times for an empty environment; want 0", n)
	}
}
