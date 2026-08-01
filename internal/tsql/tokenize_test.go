package tsql

import "testing"

// kinds renders a token stream compactly so a table test can assert shape
// without spelling out every offset.
func kinds(t *testing.T, src string) []Kind {
	t.Helper()
	toks, err := Tokenize(src)
	if err != nil {
		t.Fatalf("Tokenize(%q): %v", src, err)
	}
	var out []Kind
	for _, tk := range toks {
		out = append(out, tk.Kind)
	}
	return out
}

// roundTrip is the invariant that makes position-based rewriting safe: the
// tokens must reconstruct the source byte for byte, with nothing dropped,
// duplicated, or reordered.
func TestTokenizeRoundTripsSource(t *testing.T) {
	for _, src := range []string{
		"select 1",
		"/* {\"app\": \"dbt\"} */ with a as (select 1) select * from a",
		"select 'it''s', N'unicode', [a]]b], \"q\"\"t\" -- trailing\n",
		"/* outer /* inner */ still outer */ select 1",
		"with a as (select 1), b as (select 2) select * from a join b on 1=1",
		"",
	} {
		toks, err := Tokenize(src)
		if err != nil {
			t.Fatalf("Tokenize(%q): %v", src, err)
		}
		var rebuilt string
		for _, tk := range toks {
			if tk.Pos != len(rebuilt) {
				t.Fatalf("%q: token %q at Pos=%d, expected %d", src, tk.Text, tk.Pos, len(rebuilt))
			}
			rebuilt += tk.Text
		}
		if rebuilt != src {
			t.Fatalf("round-trip lost text:\n got %q\nwant %q", rebuilt, src)
		}
	}
}

func TestTokenizeStringLiteralsAndEscapes(t *testing.T) {
	toks, err := Tokenize(`'it''s' N'uni' [a]]b] "q""t"`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tk := range toks {
		if !tk.Trivia() {
			got = append(got, tk.Text)
		}
	}
	want := []string{`'it''s'`, `N'uni'`, `[a]]b]`, `"q""t"`}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// T-SQL nests block comments; terminating at the first */ would lex the
// remainder of the comment as code.
func TestTokenizeNestedBlockComment(t *testing.T) {
	got := kinds(t, "/* a /* b */ c */select 1")
	if got[0] != Comment {
		t.Fatalf("first token kind = %v, want comment", got[0])
	}
	toks, _ := Tokenize("/* a /* b */ c */select 1")
	if toks[0].Text != "/* a /* b */ c */" {
		t.Fatalf("comment not nested-aware: %q", toks[0].Text)
	}
}

func TestTokenizeUnterminatedIsAnError(t *testing.T) {
	for _, src := range []string{
		"select 'unterminated",
		"select [unterminated",
		`select "unterminated`,
		"/* unterminated",
		"/* outer /* inner */",
	} {
		if _, err := Tokenize(src); err == nil {
			t.Fatalf("Tokenize(%q) = nil error, want failure", src)
		}
	}
}

func TestTokenizeWordBytes(t *testing.T) {
	toks, err := Tokenize("@var #tmp a_1$")
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range toks {
		if tk.Trivia() {
			continue
		}
		if tk.Kind != Word {
			t.Fatalf("%q classified as %v, want word", tk.Text, tk.Kind)
		}
	}
}

func TestKindString(t *testing.T) {
	for k, want := range map[Kind]string{
		Space: "space", Comment: "comment", String: "string",
		QuotedIdent: "quoted-ident", Word: "word", Punct: "punct",
	} {
		if got := k.String(); got != want {
			t.Fatalf("Kind(%d).String() = %q, want %q", k, got, want)
		}
	}
}

// FuzzTokenize guards the two properties the rewriter depends on, against
// input this package does not control: the tokeniser never panics, and when it
// succeeds its tokens reconstruct the source exactly. A rewriter splices by
// offset, so a lost or shifted byte is a corrupted statement.
func FuzzTokenize(f *testing.F) {
	for _, seed := range []string{
		"with a as (with b as (select 1) select * from b) select * from a",
		"/* {\"app\": \"dbt\"} */ ;WITH x AS (SELECT 'it''s')",
		"select [a]]b], N'u', \"q\"\"t\" -- c",
		"/* outer /* inner */ */",
		"with", "((((", "'", "[", "\"", "",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, src string) {
		toks, err := Tokenize(src)
		if err != nil {
			return // rejecting malformed input is a valid outcome
		}
		rebuilt := make([]byte, 0, len(src))
		for _, tk := range toks {
			if tk.Pos != len(rebuilt) {
				t.Fatalf("token %q at Pos=%d, expected %d", tk.Text, tk.Pos, len(rebuilt))
			}
			rebuilt = append(rebuilt, tk.Text...)
		}
		if string(rebuilt) != src {
			t.Fatalf("round-trip mismatch:\n got %q\nwant %q", rebuilt, src)
		}
		// Parse builds on the same tokens; it must not panic either.
		if _, err := Parse(src); err != nil {
			return
		}
	})
}
