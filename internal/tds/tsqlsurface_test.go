package tds

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Microsoft's T-SQL surface-area page lists what a Fabric SQL analytics endpoint
// does not support. third_party/fabric-tsql-surface holds the page pinned, and a
// table classifying each of its Limitations against this endpoint's write guard.
// Three ways to fail, all of them silent before: a Limitations bullet nobody
// classified, a row whose recorded behaviour is not what the guard does, and a
// page whose bytes are not the ones pinned.

const surfaceDir = "../../third_party/fabric-tsql-surface"

type surfaceRow struct {
	Text     string   `json:"text"`
	Endpoint string   `json:"endpoint"`
	Probes   []string `json:"probes"`
	Reason   string   `json:"reason"`
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

func TestTheGuardDoesWhatEachLimitationRecords(t *testing.T) {
	_, rows := loadSurface(t)
	for _, r := range rows {
		t.Run(r.Text, func(t *testing.T) {
			if strings.TrimSpace(r.Reason) == "" {
				t.Fatal("every row carries a written reason")
			}
			switch r.Endpoint {
			case "unspecified":
				if len(r.Probes) != 0 {
					t.Fatal("an unspecified row has nothing to probe")
				}
				return
			case "refused", "forwarded":
			default:
				t.Fatalf("endpoint %q is not refused, forwarded or unspecified", r.Endpoint)
			}
			if len(r.Probes) == 0 {
				t.Fatal("a classified row needs a probe statement")
			}
			for _, q := range r.Probes {
				if got := isEndpointWrite(q); got != (r.Endpoint == "refused") {
					t.Errorf("%q: the guard refuses=%v, the table records %s", q, got, r.Endpoint)
				}
			}
		})
	}
}
