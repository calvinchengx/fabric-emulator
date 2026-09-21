package tds

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// Microsoft's T-SQL surface-area page lists what a Fabric SQL analytics endpoint
// does not support. third_party/fabric-tsql-surface holds the page pinned, and a
// table classifying each of its Limitations against the two things here that can
// refuse one: the lakehouse endpoint's write guard, which is always on, and Class
// B strict mode (-tsql-strict), which is off by default. Three ways to fail, all
// of them silent before: a Limitations bullet nobody classified, a row whose
// recorded behaviour is not what the guard or strict mode does, and a page whose
// bytes are not the ones pinned.

const surfaceDir = "../../third_party/fabric-tsql-surface"

type surfaceRow struct {
	Text    string   `json:"text"`
	Guard   string   `json:"guard"`  // refused | forwarded | unspecified
	Strict  string   `json:"strict"` // refused | not-enforced | unspecified
	Feature string   `json:"feature"`
	Probes  []string `json:"probes"`
	Reason  string   `json:"reason"`
}

func loadSurface(t *testing.T) (page string, rows []surfaceRow) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(surfaceDir, "tsql-surface-area.md"))
	if err != nil {
		t.Fatal(err)
	}
	table, err := os.ReadFile(filepath.Join(surfaceDir, "unsupported.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Unsupported []surfaceRow `json:"unsupported"`
	}
	if err := json.Unmarshal(table, &doc); err != nil {
		t.Fatal(err)
	}
	return string(raw), doc.Unsupported
}

func TestSurfacePageIsThePinnedOne(t *testing.T) {
	page, _ := loadSurface(t)
	prov, err := os.ReadFile(filepath.Join(surfaceDir, "PROVENANCE.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("`sha256:([0-9a-f]{64})`").FindSubmatch(prov)
	if m == nil {
		t.Fatal("PROVENANCE.md carries no sha256 pin")
	}
	sum := sha256.Sum256([]byte(page))
	if got := hex.EncodeToString(sum[:]); got != string(m[1]) {
		t.Fatalf("tsql-surface-area.md is %s; PROVENANCE.md pins %s", got, m[1])
	}
}

func TestEveryDocumentedLimitationIsClassified(t *testing.T) {
	page, rows := loadSurface(t)
	_, after, ok := strings.Cut(page, "### Limitations")
	if !ok {
		t.Fatal("the page has no Limitations heading any more; the table's anchor is gone")
	}
	section, _, _ := strings.Cut(after, "\n## ")
	var bullets []string
	for _, l := range strings.Split(section, "\n") {
		if b, ok := strings.CutPrefix(l, "- "); ok {
			bullets = append(bullets, b)
		}
	}
	if len(bullets) != len(rows) {
		t.Fatalf("the page lists %d limitations and the table classifies %d", len(bullets), len(rows))
	}
	for i, b := range bullets {
		if rows[i].Text != b {
			t.Errorf("row %d is %q; the page says %q", i, rows[i].Text, b)
		}
	}
}

func TestTheGuardAndStrictModeDoWhatEachLimitationRecords(t *testing.T) {
	_, rows := loadSurface(t)
	for _, r := range rows {
		t.Run(r.Text, func(t *testing.T) {
			if strings.TrimSpace(r.Reason) == "" {
				t.Fatal("every row carries a written reason")
			}
			if (r.Guard == "unspecified") != (r.Strict == "unspecified") {
				t.Fatalf("guard %q and strict %q: a sentence naming no statement is unspecified for both", r.Guard, r.Strict)
			}
			if r.Guard == "unspecified" {
				if len(r.Probes) != 0 || r.Feature != "" {
					t.Fatal("an unspecified row has nothing to probe")
				}
				return
			}
			if r.Guard != "refused" && r.Guard != "forwarded" {
				t.Fatalf("guard %q is not refused, forwarded or unspecified", r.Guard)
			}
			if r.Strict != "refused" && r.Strict != "not-enforced" {
				t.Fatalf("strict %q is not refused, not-enforced or unspecified", r.Strict)
			}
			if len(r.Probes) == 0 {
				t.Fatal("a classified row needs a probe statement")
			}
			if (r.Strict == "refused") != (r.Feature != "") {
				t.Fatalf("strict %q with feature %q: a refusal names its feature, and only a refusal does", r.Strict, r.Feature)
			}
			for _, q := range r.Probes {
				if got := isEndpointWrite(q); got != (r.Guard == "refused") {
					t.Errorf("%q: the guard refuses=%v, the table records %s", q, got, r.Guard)
				}
				err := tsql.CheckStrict(q)
				if (err != nil) != (r.Strict == "refused") {
					t.Errorf("%q: strict mode refuses=%v, the table records %s", q, err != nil, r.Strict)
					continue
				}
				var ue *tsql.UnsupportedError
				if errors.As(err, &ue) && ue.Feature != r.Feature {
					t.Errorf("%q: strict mode names %q, the table records %q", q, ue.Feature, r.Feature)
				}
			}
		})
	}
}

// What the table says in total, so a reader need not count: the guard alone
// refuses 6 of 16 and only on the endpoint; strict mode refuses 10; together they
// refuse 12; three are refused by neither and one names no statement. If a row
// changes, this changes, and the docs that quote it have to follow.
func TestWhatTheTwoRefuseTogether(t *testing.T) {
	_, rows := loadSurface(t)
	var guard, strict, either, neither, unspecified int
	for _, r := range rows {
		g, s := r.Guard == "refused", r.Strict == "refused"
		switch {
		case r.Guard == "unspecified":
			unspecified++
			continue
		case !g && !s:
			neither++
		}
		if g {
			guard++
		}
		if s {
			strict++
		}
		if g || s {
			either++
		}
	}
	if guard != 6 || strict != 10 || either != 12 || neither != 3 || unspecified != 1 {
		t.Errorf("guard %d, strict %d, either %d, neither %d, unspecified %d; want 6, 10, 12, 3, 1", guard, strict, either, neither, unspecified)
	}
}
