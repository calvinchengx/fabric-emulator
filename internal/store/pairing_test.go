package store

// PairItems is a pure function, so the cases the database cannot currently
// produce (duplicate name+type in one workspace) are still tested here — see
// the note at the top of pairing.go for why that matters.

import (
	"reflect"
	"testing"
)

func it(id, name, typ string) *Item {
	return &Item{ID: id, DisplayName: name, Type: typ}
}

// names renders pairs as "earlierID>laterID" for compact comparison.
func names(pairs [][2]*Item) []string {
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p[0].ID+">"+p[1].ID)
	}
	return out
}

func TestPairItemsMatchesNameAndType(t *testing.T) {
	earlier := []*Item{it("e1", "orders", "Notebook"), it("e2", "sales", "Lakehouse")}
	later := []*Item{it("l1", "orders", "Notebook"), it("l2", "sales", "Lakehouse")}
	pairs, ambiguous := PairItems(earlier, later)
	if len(ambiguous) != 0 {
		t.Fatalf("ambiguous = %v", ambiguous)
	}
	if got := names(pairs); !reflect.DeepEqual(got, []string{"e1>l1", "e2>l2"}) {
		t.Fatalf("pairs = %v", got)
	}
}

// TestPairItemsTypeIsPartOfTheKey: same name, different type is NOT a pair.
func TestPairItemsTypeIsPartOfTheKey(t *testing.T) {
	pairs, ambiguous := PairItems(
		[]*Item{it("e1", "orders", "Notebook")},
		[]*Item{it("l1", "orders", "Lakehouse")})
	if len(pairs) != 0 || len(ambiguous) != 0 {
		t.Fatalf("pairs = %v, ambiguous = %v; want neither", names(pairs), ambiguous)
	}
}

// TestPairItemsCaseInsensitive: the uniqueness index is COLLATE NOCASE, so
// matching has to agree with it or the two disagree about what a duplicate is.
func TestPairItemsCaseInsensitive(t *testing.T) {
	pairs, ambiguous := PairItems(
		[]*Item{it("e1", "Orders", "Notebook")},
		[]*Item{it("l1", "orders", "NOTEBOOK")})
	if len(ambiguous) != 0 {
		t.Fatalf("ambiguous = %v", ambiguous)
	}
	if got := names(pairs); !reflect.DeepEqual(got, []string{"e1>l1"}) {
		t.Fatalf("pairs = %v", got)
	}
}

// TestPairItemsUnmatchedStayUnpaired: items on only one side are simply not
// paired — they are not an error, and a later deploy clean-deploys them.
func TestPairItemsUnmatchedStayUnpaired(t *testing.T) {
	pairs, ambiguous := PairItems(
		[]*Item{it("e1", "orders", "Notebook"), it("e2", "only-here", "Notebook")},
		[]*Item{it("l1", "orders", "Notebook"), it("l2", "only-there", "Notebook")})
	if len(ambiguous) != 0 {
		t.Fatalf("ambiguous = %v", ambiguous)
	}
	if got := names(pairs); !reflect.DeepEqual(got, []string{"e1>l1"}) {
		t.Fatalf("pairs = %v", got)
	}
}

// TestPairItemsAmbiguous: duplicates on either side refuse to pair rather
// than guessing. Unreachable through the store today (the UNIQUE index
// forbids it), which is exactly why it is tested directly.
func TestPairItemsAmbiguous(t *testing.T) {
	for name, tc := range map[string]struct{ earlier, later []*Item }{
		"duplicate on the earlier side": {
			earlier: []*Item{it("e1", "orders", "Notebook"), it("e2", "orders", "Notebook")},
			later:   []*Item{it("l1", "orders", "Notebook")},
		},
		"duplicate on the later side": {
			earlier: []*Item{it("e1", "orders", "Notebook")},
			later:   []*Item{it("l1", "orders", "Notebook"), it("l2", "orders", "Notebook")},
		},
		"duplicate on both sides": {
			earlier: []*Item{it("e1", "orders", "Notebook"), it("e2", "orders", "Notebook")},
			later:   []*Item{it("l1", "orders", "Notebook"), it("l2", "orders", "Notebook")},
		},
	} {
		pairs, ambiguous := PairItems(tc.earlier, tc.later)
		if len(pairs) != 0 {
			t.Errorf("%s: paired anyway: %v", name, names(pairs))
		}
		if len(ambiguous) != 1 || ambiguous[0] != "orders (Notebook)" {
			t.Errorf("%s: ambiguous = %v", name, ambiguous)
		}
	}
}

// TestPairItemsDeterministic: a stable order, so pair rows and any report
// built from them don't shuffle between runs.
func TestPairItemsDeterministic(t *testing.T) {
	earlier := []*Item{it("e1", "zeta", "Notebook"), it("e2", "alpha", "Notebook")}
	later := []*Item{it("l1", "zeta", "Notebook"), it("l2", "alpha", "Notebook")}
	want := []string{"e2>l2", "e1>l1"} // alpha before zeta
	for i := 0; i < 5; i++ {
		if got := names(mustPairs(t, earlier, later)); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: pairs = %v, want %v", i, got, want)
		}
	}
}

func mustPairs(t *testing.T, earlier, later []*Item) [][2]*Item {
	t.Helper()
	pairs, ambiguous := PairItems(earlier, later)
	if len(ambiguous) != 0 {
		t.Fatalf("unexpected ambiguity: %v", ambiguous)
	}
	return pairs
}

func TestPairItemsEmpty(t *testing.T) {
	pairs, ambiguous := PairItems(nil, nil)
	if len(pairs) != 0 || len(ambiguous) != 0 {
		t.Fatalf("pairs = %v, ambiguous = %v", names(pairs), ambiguous)
	}
	pairs, _ = PairItems([]*Item{it("e1", "a", "Notebook")}, nil)
	if len(pairs) != 0 {
		t.Fatalf("paired against an empty stage: %v", names(pairs))
	}
}
