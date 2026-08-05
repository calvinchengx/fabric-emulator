package store

import (
	"os"
	"regexp"
	"slices"
	"testing"
)

// TestAllKindsHoldsEveryDeclaredKind checks the contract against something
// OTHER than itself.
//
// A test that iterates AllKinds and asserts each entry is a valid kind passes
// whatever that slice happens to say — including a slice that has silently
// fallen behind. The failure worth catching is exactly that: someone adds
// `KindSchedule = "schedule"`, forgets the slice, and the generated client never
// subscribes to it. Nothing raises, because the stream names each frame and
// EventSource has no wildcard listener.
//
// So this reads the SOURCE for `Kind… = "…"` declarations and requires each one
// to appear. The source is the independent witness.
func TestAllKindsHoldsEveryDeclaredKind(t *testing.T) {
	src, err := os.ReadFile("bus.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := regexp.MustCompile(`(?m)^\s+Kind\w+\s+=\s+"([a-z]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(declared) == 0 {
		t.Fatal("found no Kind… constants in bus.go — this test is reading the " +
			"wrong thing and would pass on anything")
	}
	for _, m := range declared {
		if !slices.Contains(AllKinds, m[1]) {
			t.Errorf("kind %q is declared but missing from AllKinds — the generated "+
				"client will never subscribe to it, and nothing else will say so", m[1])
		}
	}
	// And the reverse: a slice entry with no constant behind it would generate a
	// subscription for a kind that can never arrive.
	if len(AllKinds) != len(declared) {
		t.Errorf("AllKinds has %d entries for %d declared constants — one of them "+
			"is describing something that does not exist", len(AllKinds), len(declared))
	}
}

// TestViewKindsIsAllKindsMinusTheSubscriberSignal.
//
// `dropped` reports on the SUBSCRIBER — this browser fell behind and lost N
// events — not on the platform. Offering it as a filter would let someone switch
// off the one signal that says the log they are reading is incomplete.
//
// Pinned in both directions: a view kind that is not a real kind, and a platform
// kind quietly missing from the filters, are both bugs.
func TestViewKindsIsAllKindsMinusTheSubscriberSignal(t *testing.T) {
	for _, k := range ViewKinds {
		if !slices.Contains(AllKinds, k) {
			t.Errorf("ViewKinds has %q, which is not a kind the bus carries", k)
		}
	}
	if slices.Contains(ViewKinds, KindDropped) {
		t.Error("dropped is offered as a filter; it reports on the subscriber, and " +
			"hiding it hides that the log is incomplete")
	}
	for _, k := range AllKinds {
		if k == KindDropped {
			continue
		}
		if !slices.Contains(ViewKinds, k) {
			t.Errorf("kind %q is not offered as a filter — a platform event the UI "+
				"cannot show is one nobody will notice is missing", k)
		}
	}
}
