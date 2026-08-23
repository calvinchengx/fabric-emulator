package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// A notebook created the DOCUMENTED way — `.ipynb` content, which is what
// `notebookutils.notebook.create(content=...)` takes — must be runnable.
//
// Before the derivation this test guards, it was not: the item stored happily,
// reported 201/202, and its RunNotebook job then died on `notebook-content.py
// is missing`, because this emulator executes from the Fabric `.py` form. A
// create that succeeds and produces something unrunnable is the false-green
// shape docs/38 §4 names, arriving one API call later than the lie.
func TestCreateNotebookFromIPYNBDerivesTheExecutablePart(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}

	ipynb := `{"cells":[{"cell_type":"code","source":["print('hi')\n"]},` +
		`{"cell_type":"markdown","source":["# title"]}],` +
		`"metadata":{},"nbformat":4,"nbformat_minor":5}`
	body, _ := json.Marshal(map[string]any{
		"displayName": "from-ipynb", "type": "Notebook",
		"definition": map[string]any{"parts": []map[string]string{{
			"path":        "notebook-content.ipynb",
			"payloadType": "InlineBase64",
			"payload":     base64.StdEncoding.EncodeToString([]byte(ipynb)),
		}}},
	})
	// 201 or 202: a create carrying a definition is asynchronous here, and the
	// item exists either way. The id comes from the store rather than the body
	// so this asserts the derivation, not the LRO shape (api_p1 owns that).
	if w := do(a.createItem, admin, "POST", string(body), wid); w.Code != http.StatusCreated &&
		w.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	it, err := st.GetItemByName(ws.ID, "from-ipynb", "Notebook")
	if err != nil {
		t.Fatalf("created notebook not found: %v", err)
	}

	// The executable part is what the run path reads. Asking for it directly is
	// the assertion: a getDefinition listing both parts would pass even if the
	// derived one were empty.
	src, err := a.notebookContent(it.ID)
	if err != nil {
		t.Fatalf("notebook-content.py missing after an .ipynb create: %v", err)
	}
	if !strings.Contains(string(src), "print('hi')") {
		t.Fatalf("derived source lost the code cell: %q", src)
	}
	if !strings.Contains(string(src), "# MAGIC # title") {
		t.Fatalf("derived source lost the markdown cell: %q", src)
	}
	// The .ipynb the caller sent is kept as sent — VS Code reads it back.
	parts, err := a.Store.GetDefinition(it.ID)
	if err != nil {
		t.Fatalf("GetDefinition: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d; want the .ipynb kept and the .py derived", len(parts))
	}
}

// An author who sends BOTH parts keeps theirs. Re-deriving would silently
// discard whatever the ipynb round trip does not carry.
func TestCreateNotebookWithBothPartsDoesNotRederive(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	wid := map[string]string{"wid": ws.ID}

	mine := "# Fabric notebook source\n\n# CELL ********************\nprint('mine')\n"
	body, _ := json.Marshal(map[string]any{
		"displayName": "both", "type": "Notebook",
		"definition": map[string]any{"parts": []map[string]string{
			{"path": "notebook-content.ipynb", "payloadType": "InlineBase64",
				"payload": base64.StdEncoding.EncodeToString([]byte(`{"cells":[]}`))},
			{"path": "notebook-content.py", "payloadType": "InlineBase64",
				"payload": base64.StdEncoding.EncodeToString([]byte(mine))},
		}},
	})
	if w := do(a.createItem, admin, "POST", string(body), wid); w.Code != http.StatusCreated &&
		w.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	it, err := st.GetItemByName(ws.ID, "both", "Notebook")
	if err != nil {
		t.Fatalf("created notebook not found: %v", err)
	}
	src, err := a.notebookContent(it.ID)
	if err != nil {
		t.Fatalf("notebookContent: %v", err)
	}
	if string(src) != mine {
		t.Fatalf("author's .py was overwritten:\n got %q\nwant %q", src, mine)
	}
}

// Only Notebook items are touched, and the edges say what they do rather than
// being left to a reader's assumption.
func TestNotebookExecutablePartsScope(t *testing.T) {
	ipynb := store.DefinitionPart{
		Path:        "notebook-content.ipynb",
		PayloadType: "InlineBase64",
		Payload:     base64.StdEncoding.EncodeToString([]byte(`{"cells":[]}`)),
	}
	if got := notebookExecutableParts("Lakehouse", []store.DefinitionPart{ipynb}); len(got) != 1 {
		t.Fatalf("a Lakehouse gained a derived part: %d", len(got))
	}
	// Case-insensitive, because the store keeps whichever spelling was sent.
	if got := notebookExecutableParts("notebook", []store.DefinitionPart{ipynb}); len(got) != 2 {
		t.Fatalf("lowercase type did not derive: %d", len(got))
	}
	if got := notebookExecutableParts("Notebook", nil); got != nil {
		t.Fatalf("no parts should stay no parts: %v", got)
	}
	// Undecodable payload is stored as sent; the create behaves exactly as it
	// did before this derivation existed, and the caller sees their own error.
	bad := store.DefinitionPart{Path: "notebook-content.ipynb", Payload: "!!not base64!!"}
	if got := notebookExecutableParts("Notebook", []store.DefinitionPart{bad}); len(got) != 1 {
		t.Fatalf("a bad payload should not produce a derived part: %d", len(got))
	}
}
