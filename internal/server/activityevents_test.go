package server_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
)

type activityPage struct {
	ActivityEventEntities []map[string]any `json:"activityEventEntities"`
	ContinuationURI       *string          `json:"continuationUri"`
	ContinuationToken     *string          `json:"continuationToken"`
	LastResultSet         bool             `json:"lastResultSet"`
}

// One day's window, single-quoted the way the API documents.
//
// The day is a PARAMETER rather than a second reading of the wall clock, and
// that is the whole repair: every caller below captures it once with
// testsupport.UTCDay BEFORE the operations whose events the window must
// contain, then widens with testsupport.UTCDaysSince at the moment it queries.
// Deriving it here instead meant two independent clock reads straddling the
// operations, so a run crossing UTC midnight between them asked for a day the
// events were not in and failed on a missing CreateWorkspace.
func activityWindow(day string) string {
	return fmt.Sprintf("startDateTime='%sT00:00:00Z'&endDateTime='%sT23:59:59Z'", day, day)
}

func (f *fixture) activity(t *testing.T, query string) activityPage {
	t.Helper()
	var page activityPage
	f.mustStatus(f.call("GET", "/v1.0/myorg/admin/activityevents?"+query, f.token, nil, &page),
		http.StatusOK, "activityevents")
	return page
}

// activitySince is every event in the single-day window of `captured`, unioned
// with the current day's when the UTC day has advanced since `captured` was
// read. The API refuses a two-day window (400 -- the `spans two days` case
// below pins it), so a straddle has to be two queries rather than one wider
// one.
func (f *fixture) activitySince(t *testing.T, captured string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, day := range testsupport.UTCDaysSince(captured) {
		out = append(out, f.activity(t, activityWindow(day)).ActivityEventEntities...)
	}
	return out
}

// The audit log is real: operations performed through the API show up in it,
// with the documented operation names.
func TestActivityEventsRecordsRealOperations(t *testing.T) {
	f := newFixture(t)
	// Captured BEFORE the operations below, so the window is derived from an
	// instant that precedes every event it has to contain.
	day := testsupport.UTCDay()

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "audited"}, &ws)
	var item struct{ ID string }
	f.call("POST", "/v1/workspaces/"+ws.ID+"/notebooks", f.token,
		map[string]any{"displayName": "nb"}, &item)
	f.call("PATCH", "/v1/workspaces/"+ws.ID+"/items/"+item.ID, f.token,
		map[string]any{"displayName": "nb-renamed"}, nil)
	f.call("DELETE", "/v1/workspaces/"+ws.ID+"/items/"+item.ID, f.token, nil, nil)

	seen := map[string]map[string]any{}
	for _, e := range f.activitySince(t, day) {
		op, _ := e["Operation"].(string)
		seen[op] = e
	}
	for _, op := range []string{"CreateWorkspace", "CreateArtifact", "UpdateArtifact", "DeleteArtifact"} {
		if _, ok := seen[op]; !ok {
			t.Fatalf("no %s event; saw %v", op, keysOf(seen))
		}
	}
	// The artifact events name what they acted on, and the delete event still
	// carries the name even though the item is gone.
	if got := seen["DeleteArtifact"]["ArtifactName"]; got != "nb-renamed" {
		t.Fatalf("DeleteArtifact ArtifactName = %v, want nb-renamed", got)
	}
	if got := seen["CreateArtifact"]["ArtifactKind"]; got != "Notebook" {
		t.Fatalf("CreateArtifact ArtifactKind = %v, want Notebook", got)
	}
	// Every event identifies its actor and carries a UTC timestamp.
	for op, e := range seen {
		if e["UserId"] == "" || e["UserId"] == nil {
			t.Fatalf("%s has no UserId", op)
		}
		ct, _ := e["CreationTime"].(string)
		if _, err := time.Parse("2006-01-02T15:04:05Z", ct); err != nil {
			t.Fatalf("%s CreationTime %q is not UTC RFC3339: %v", op, ct, err)
		}
	}
}

// Domain operations use the names and operationProperties the domain audit
// schema documents — not the generic CRUD vocabulary.
func TestActivityEventsUsesDocumentedDomainVocabulary(t *testing.T) {
	f := newFixture(t)
	// Captured before the domain operations, for the reason given on
	// activityWindow.
	day := testsupport.UTCDay()

	var parent, child domainResp
	f.call("POST", "/v1/admin/domains", f.token, map[string]any{"displayName": "Audit-Parent"}, &parent)
	f.call("POST", "/v1/admin/domains", f.token,
		map[string]any{"displayName": "Audit-Child", "parentDomainId": parent.ID}, &child)
	f.call("PATCH", "/v1/admin/domains/"+parent.ID, f.token,
		map[string]any{"description": "changed"}, nil)

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "domain-ws"}, &ws)
	f.call("POST", "/v1/admin/domains/"+parent.ID+"/assignWorkspaces", f.token,
		map[string]any{"workspacesIds": []string{ws.ID}}, nil)
	f.call("POST", "/v1/admin/domains/"+parent.ID+"/unassignAllWorkspaces", f.token, nil, nil)
	f.call("DELETE", "/v1/admin/domains/"+parent.ID, f.token, nil, nil)

	byOp := map[string]map[string]any{}
	for _, e := range f.activitySince(t, day) {
		if op, _ := e["Operation"].(string); op != "" {
			byOp[op] = e
		}
	}
	for _, op := range []string{
		"InsertDataDomainAsAdmin",
		"UpdateDataDomainAsAdmin",
		"UpdateDataDomainFoldersRelationsAsAdmin",
		"DeleteAllDataDomainFoldersRelationsAsAdmin",
		"DeleteDataDomainAsAdmin",
	} {
		e, ok := byOp[op]
		if !ok {
			t.Fatalf("no %s event; saw %v", op, keysOf(byOp))
		}
		if e["DataDomainObjectId"] == nil || e["DataDomainDisplayName"] == nil {
			t.Fatalf("%s missing documented properties: %v", op, e)
		}
	}
	// A subdomain's create event carries ParentObjectId; a root domain's
	// does not.
	if got := byOp["InsertDataDomainAsAdmin"]["DataDomainDisplayName"]; got != "Audit-Child" {
		t.Fatalf("last insert event = %v, want the subdomain", got)
	}
	if byOp["InsertDataDomainAsAdmin"]["ParentObjectId"] != parent.ID {
		t.Fatalf("subdomain insert missing ParentObjectId: %v", byOp["InsertDataDomainAsAdmin"])
	}
	// The assignment event carries the documented counter.
	if byOp["UpdateDataDomainFoldersRelationsAsAdmin"]["FoldersToSetCounter"] == nil {
		t.Fatalf("assignment event missing FoldersToSetCounter: %v",
			byOp["UpdateDataDomainFoldersRelationsAsAdmin"])
	}
}

// The documented request rules: quoted UTC DateTimes, both on the same day.
func TestActivityEventsRequestValidation(t *testing.T) {
	f := newFixture(t)
	day := time.Now().UTC().Format("2006-01-02")

	for _, tc := range []struct{ name, query string }{
		{"no bounds", ""},
		{"only start", "startDateTime='" + day + "T00:00:00Z'"},
		{"unparseable", "startDateTime='yesterday'&endDateTime='today'"},
		{"end before start", fmt.Sprintf("startDateTime='%sT10:00:00Z'&endDateTime='%sT09:00:00Z'", day, day)},
		{"spans two days", fmt.Sprintf("startDateTime='%sT00:00:00Z'&endDateTime='2999-01-01T00:00:00Z'", day)},
		{"bad token", "continuationToken='not-a-real-token'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.mustStatus(f.call("GET", "/v1.0/myorg/admin/activityevents?"+tc.query, f.token, nil, nil),
				http.StatusBadRequest, tc.name)
		})
	}

	// Unquoted values are accepted too — the quotes are how the docs write
	// them, not a parsing requirement.
	f.mustStatus(f.call("GET", fmt.Sprintf(
		"/v1.0/myorg/admin/activityevents?startDateTime=%sT00:00:00Z&endDateTime=%sT23:59:59Z", day, day),
		f.token, nil, nil), http.StatusOK, "unquoted bounds")

	// A window with no events returns an empty array and no token, rather
	// than null or an error.
	page := f.activity(t, "startDateTime='2001-01-01T00:00:00Z'&endDateTime='2001-01-01T23:59:59Z'")
	if len(page.ActivityEventEntities) != 0 || page.ContinuationToken != nil {
		t.Fatalf("empty window returned %+v", page)
	}
}

// Paging: follow continuationToken until it stops coming back, exactly as the
// documented client loop does, and confirm no event is dropped or repeated.
func TestActivityEventsPagination(t *testing.T) {
	f := newFixture(t)
	// Captured before the 210 creates, for the reason given on activityWindow.
	// This test is the one a straddle hurts most: the events would be SPLIT
	// across the two days rather than merely absent from one, so a single-day
	// window would come back short rather than empty and the failure would read
	// as a paging bug.
	day := testsupport.UTCDay()

	// 210 workspaces > the 200-entry page size, so at least two pages.
	const want = 210
	for i := 0; i < want; i++ {
		f.call("POST", "/v1/workspaces", f.token,
			map[string]string{"displayName": fmt.Sprintf("page-ws-%03d", i)}, nil)
	}

	ids := map[string]bool{}
	pages := 0
	for _, d := range testsupport.UTCDaysSince(day) {
		page := f.activity(t, activityWindow(d))
		for {
			pages++
			for _, e := range page.ActivityEventEntities {
				id, _ := e["Id"].(string)
				if id == "" {
					t.Fatal("event without an Id")
				}
				if ids[id] {
					t.Fatalf("event %s repeated across pages", id)
				}
				ids[id] = true
			}
			if page.ContinuationToken == nil {
				if page.ContinuationURI != nil {
					t.Fatalf("final page has no token but a continuationUri: %+v", page.ContinuationURI)
				}
				break
			}
			// The continuationUri is a usable URL carrying that same token.
			if page.ContinuationURI == nil {
				t.Fatal("page has a token but no continuationUri")
			}
			u, err := url.Parse(*page.ContinuationURI)
			if err != nil {
				t.Fatalf("continuationUri is not a URL: %v", err)
			}
			if got := u.Query().Get("continuationToken"); got != "'"+*page.ContinuationToken+"'" {
				t.Fatalf("continuationUri token = %q, want the page token", got)
			}
			if pages > 10 {
				t.Fatal("continuation loop did not terminate")
			}
			page = f.activity(t, "continuationToken='"+*page.ContinuationToken+"'")
		}
	}
	if pages < 2 {
		t.Fatalf("expected multiple pages, got %d", pages)
	}
	if len(ids) < want {
		t.Fatalf("collected %d events across %d pages, want at least %d", len(ids), pages, want)
	}
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
