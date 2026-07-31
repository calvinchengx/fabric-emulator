package server_test

import (
	"net/http"
	"testing"
)

type labelList struct {
	Labels []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Order int    `json:"order"`
	} `json:"labels"`
}

type bulkResult struct {
	SuccessfulItems []struct{ ID string }        `json:"successfulItems"`
	FailedItems     []struct{ ID, Error string } `json:"failedItems"`
}

// labelsByName indexes the taxonomy so tests name labels rather than guids.
func (f *fixture) labels(t *testing.T) map[string]string {
	t.Helper()
	var out labelList
	f.mustStatus(f.call("GET", "/v1/admin/labels", f.token, nil, &out), http.StatusOK, "list labels")
	byName := map[string]string{}
	for _, l := range out.Labels {
		byName[l.Name] = l.ID
	}
	return byName
}

// itemLabel reads back the label the item carries, or "" when it has none.
func (f *fixture) itemLabel(t *testing.T, wsID, itemID string) string {
	t.Helper()
	var got struct {
		Properties struct {
			SensitivityLabel struct {
				LabelID string `json:"labelId"`
			} `json:"sensitivityLabel"`
		} `json:"properties"`
	}
	f.mustStatus(f.call("GET", "/v1/workspaces/"+wsID+"/items/"+itemID, f.token, nil, &got),
		http.StatusOK, "get item")
	return got.Properties.SensitivityLabel.LabelID
}

// Applying, replacing and clearing a label, through the documented bulk APIs.
func TestBulkLabelsApplyReplaceAndRemove(t *testing.T) {
	f := newFixture(t)
	labels := f.labels(t)
	if len(labels) < 4 {
		t.Fatalf("taxonomy = %v, want at least 4 labels", labels)
	}

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "labelled"}, &ws)
	var a, b struct{ ID string }
	f.call("POST", "/v1/workspaces/"+ws.ID+"/notebooks", f.token, map[string]any{"displayName": "nb-a"}, &a)
	f.call("POST", "/v1/workspaces/"+ws.ID+"/notebooks", f.token, map[string]any{"displayName": "nb-b"}, &b)

	// Apply to both items in one call.
	var res bulkResult
	f.mustStatus(f.call("POST", "/v1/admin/items/bulkSetLabels", f.token, map[string]any{
		"labelId": labels["Confidential"],
		"items":   []map[string]string{{"id": a.ID}, {"id": b.ID}},
	}, &res), http.StatusOK, "bulkSetLabels")
	if len(res.SuccessfulItems) != 2 || len(res.FailedItems) != 0 {
		t.Fatalf("bulk set = %+v", res)
	}
	if got := f.itemLabel(t, ws.ID, a.ID); got != labels["Confidential"] {
		t.Fatalf("item label = %q, want Confidential", got)
	}

	// Replace on one of them.
	f.mustStatus(f.call("POST", "/v1/admin/items/bulkSetLabels", f.token, map[string]any{
		"labelId": labels["Public"],
		"items":   []map[string]string{{"id": a.ID}},
	}, nil), http.StatusOK, "replace label")
	if got := f.itemLabel(t, ws.ID, a.ID); got != labels["Public"] {
		t.Fatalf("after replace, label = %q, want Public", got)
	}
	// The other item is untouched.
	if got := f.itemLabel(t, ws.ID, b.ID); got != labels["Confidential"] {
		t.Fatalf("sibling label changed to %q", got)
	}

	// Remove.
	res = bulkResult{}
	f.mustStatus(f.call("POST", "/v1/admin/items/bulkRemoveLabels", f.token, map[string]any{
		"items": []map[string]string{{"id": a.ID}},
	}, &res), http.StatusOK, "bulkRemoveLabels")
	if len(res.SuccessfulItems) != 1 {
		t.Fatalf("bulk remove = %+v", res)
	}
	if got := f.itemLabel(t, ws.ID, a.ID); got != "" {
		t.Fatalf("label survived removal: %q", got)
	}
	// Removing again is not an error — it is simply not a change.
	f.mustStatus(f.call("POST", "/v1/admin/items/bulkRemoveLabels", f.token, map[string]any{
		"items": []map[string]string{{"id": a.ID}},
	}, nil), http.StatusOK, "remove twice")
}

// Bulk APIs report per-item failures rather than failing the whole call.
func TestBulkLabelsPartialFailureAndValidation(t *testing.T) {
	f := newFixture(t)
	labels := f.labels(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "partial"}, &ws)
	var ok struct{ ID string }
	f.call("POST", "/v1/workspaces/"+ws.ID+"/notebooks", f.token, map[string]any{"displayName": "nb"}, &ok)

	var res bulkResult
	f.mustStatus(f.call("POST", "/v1/admin/items/bulkSetLabels", f.token, map[string]any{
		"labelId": labels["General"],
		"items": []map[string]string{
			{"id": ok.ID}, {"id": "00000000-0000-4000-8000-000000000000"},
		},
	}, &res), http.StatusOK, "mixed bulk")
	if len(res.SuccessfulItems) != 1 || len(res.FailedItems) != 1 {
		t.Fatalf("mixed bulk = %+v", res)
	}
	if res.FailedItems[0].Error != "ItemNotFound" {
		t.Fatalf("failure reason = %q", res.FailedItems[0].Error)
	}

	// An unknown label is a request-level error, not a per-item one.
	f.mustStatus(f.call("POST", "/v1/admin/items/bulkSetLabels", f.token, map[string]any{
		"labelId": "99999999-0000-4000-8000-000000000000",
		"items":   []map[string]string{{"id": ok.ID}},
	}, nil), http.StatusNotFound, "unknown label")
	// Missing required fields.
	f.mustStatus(f.call("POST", "/v1/admin/items/bulkSetLabels", f.token,
		map[string]any{"labelId": labels["General"]}, nil), http.StatusBadRequest, "no items")
	f.mustStatus(f.call("POST", "/v1/admin/items/bulkSetLabels", f.token,
		map[string]any{"items": []map[string]string{{"id": ok.ID}}}, nil),
		http.StatusBadRequest, "no labelId")
	f.mustStatus(f.call("POST", "/v1/admin/items/bulkRemoveLabels", f.token,
		map[string]any{}, nil), http.StatusBadRequest, "remove with no items")
}

// Label changes emit the documented SensitivityLabelEventData, including the
// upgrade/downgrade classification that depends on label order.
func TestLabelChangesAuditWithDocumentedSchema(t *testing.T) {
	f := newFixture(t)
	labels := f.labels(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "label-audit"}, &ws)
	var it struct{ ID string }
	f.call("POST", "/v1/workspaces/"+ws.ID+"/notebooks", f.token, map[string]any{"displayName": "nb"}, &it)

	set := func(name string) {
		f.call("POST", "/v1/admin/items/bulkSetLabels", f.token, map[string]any{
			"labelId": labels[name], "items": []map[string]string{{"id": it.ID}},
		}, nil)
	}
	set("General")             // applied (no previous label)
	set("Highly Confidential") // upgraded
	set("Public")              // downgraded
	f.call("POST", "/v1/admin/items/bulkRemoveLabels", f.token,
		map[string]any{"items": []map[string]string{{"id": it.ID}}}, nil)

	// Collect this item's label events in order.
	var events []map[string]any
	for _, e := range f.activity(t, activityWindow()).ActivityEventEntities {
		op, _ := e["Operation"].(string)
		if e["ArtifactId"] == it.ID &&
			(op == "SensitivityLabelApplied" || op == "SensitivityLabelChanged" || op == "SensitivityLabelRemoved") {
			events = append(events, e)
		}
	}
	if len(events) != 4 {
		t.Fatalf("got %d label events, want 4: %+v", len(events), events)
	}

	// LabelEventType: 1 upgraded, 2 downgraded, 3 removed, 4 same order.
	want := []struct {
		op        string
		eventType float64
		hasOld    bool
		hasNew    bool
	}{
		{"SensitivityLabelApplied", 1, false, true}, // no previous label → upgrade
		{"SensitivityLabelChanged", 1, true, true},  // General → Highly Confidential
		{"SensitivityLabelChanged", 2, true, true},  // Highly Confidential → Public
		{"SensitivityLabelRemoved", 3, true, false},
	}
	for i, w := range want {
		e := events[i]
		if e["Operation"] != w.op {
			t.Fatalf("event %d Operation = %v, want %s", i, e["Operation"], w.op)
		}
		if e["LabelEventType"] != w.eventType {
			t.Fatalf("event %d (%s) LabelEventType = %v, want %v", i, w.op, e["LabelEventType"], w.eventType)
		}
		if _, ok := e["OldSensitivityLabelId"]; ok != w.hasOld {
			t.Fatalf("event %d (%s) OldSensitivityLabelId present = %v, want %v", i, w.op, ok, w.hasOld)
		}
		if _, ok := e["SensitivityLabelId"]; ok != w.hasNew {
			t.Fatalf("event %d (%s) SensitivityLabelId present = %v, want %v", i, w.op, ok, w.hasNew)
		}
		// ActionSource 3 = Manual, ActionSourceDetail 5 = PublicAPI,
		// ArtifactType 12 = Fabric item — all from the audit schema.
		if e["ActionSource"] != float64(3) || e["ActionSourceDetail"] != float64(5) ||
			e["ArtifactType"] != float64(12) {
			t.Fatalf("event %d (%s) enum fields = %v/%v/%v, want 3/5/12",
				i, w.op, e["ActionSource"], e["ActionSourceDetail"], e["ArtifactType"])
		}
	}
}
