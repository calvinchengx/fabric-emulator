package api

// A deleted display name is not free yet, when the operator asks for that.
//
// MEASURED on a tenant 2026-08-11: create a Notebook, delete it, recreate it
// under the same name →
//
//	409 ItemDisplayNameNotAvailableYet
//	"Requested 'emuNameProbe' is not available yet and is expected to become
//	 available in the upcoming minutes."
//	isRetriable: true
//
// and the name was free again on the retry 20 seconds later — so the message's
// "upcoming minutes" is not a duration anyone should encode. The window is the
// operator's, and the default is off.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func TestDeletedNameIsHeldOnlyWhenAWindowIsConfigured(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "reused"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteItem(ws.ID, it.ID); err != nil {
		t.Fatal(err)
	}

	// DEFAULT: no window, the name is free at once. This is the control — the
	// case below only means something if the reservation is opt-in.
	body := `{"displayName":"reused","type":"Notebook"}`
	if w := do(a.createItem, admin, "POST", body, map[string]string{"wid": ws.ID}); w.Code >= 300 {
		t.Fatalf("with no window configured the name was held: %d %s", w.Code, w.Body.Bytes())
	}
}

func TestDeletedNameIsRefusedAsRetriableWithinTheWindow(t *testing.T) {
	a, st := newAPI(t)
	a.NameReservation = time.Hour
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "reused"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteItem(ws.ID, it.ID); err != nil {
		t.Fatal(err)
	}

	w := do(a.createItem, admin, "POST",
		`{"displayName":"reused","type":"Notebook"}`, map[string]string{"wid": ws.ID})
	if w.Code != http.StatusConflict {
		t.Fatalf("reserved name = %d, want 409", w.Code)
	}
	var body struct {
		ErrorCode   string `json:"errorCode"`
		Message     string `json:"message"`
		IsRetriable *bool  `json:"isRetriable"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.ErrorCode != "ItemDisplayNameNotAvailableYet" {
		t.Errorf("errorCode = %q", body.ErrorCode)
	}
	// The distinguishing field. A name held after a delete and a name taken by
	// a live item are BOTH 409, and only this says which — a client treating
	// them alike gives up on the one that succeeds seconds later.
	if body.IsRetriable == nil || !*body.IsRetriable {
		t.Errorf("isRetriable = %v, want true", body.IsRetriable)
	}
	if body.ErrorCode == "ItemDisplayNameAlreadyInUse" {
		t.Error("reported a live-name conflict for a name nothing holds")
	}

	// A DIFFERENT type is unaffected: the reservation is per workspace+type+name,
	// as the conflict rule above it already is.
	if w := do(a.createItem, admin, "POST",
		`{"displayName":"reused","type":"Lakehouse"}`, map[string]string{"wid": ws.ID}); w.Code >= 300 {
		t.Errorf("another type was blocked by the reservation: %d %s", w.Code, w.Body.Bytes())
	}
}

// A live name still reports the conflict it always did, so the new branch has
// not swallowed the old one.
func TestALiveNameStillReportsAlreadyInUse(t *testing.T) {
	a, st := newAPI(t)
	a.NameReservation = time.Hour
	ws := seedWorkspace(t, st)
	if err := st.CreateItem(&store.Item{
		WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	w := do(a.createItem, admin, "POST",
		`{"displayName":"live","type":"Notebook"}`, map[string]string{"wid": ws.ID})
	if w.Code != http.StatusConflict || errorCode(t, w) != "ItemDisplayNameAlreadyInUse" {
		t.Fatalf("live name = %d %s", w.Code, w.Body.Bytes())
	}
}

// The window expires. Asserted through the store's own clock rather than by
// sleeping, so the test states the property instead of racing it.
func TestTheReservationExpires(t *testing.T) {
	a, st := newAPI(t)
	a.NameReservation = time.Minute
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "expiring"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteItem(ws.ID, it.ID); err != nil {
		t.Fatal(err)
	}
	if free := st.NameReservedUntil(ws.ID, "Notebook", "expiring", time.Minute); free.IsZero() {
		t.Fatal("name not held immediately after delete")
	}

	// ADVANCE THE EMULATOR CLOCK rather than shrink the window. Passing a tiny
	// window looks like the same test and is not: with time frozen, a 1ns
	// window still ends 1ns in the future, so it stays held and the assertion
	// fails against correct code. The property is "the window elapses", and
	// only moving the clock states it.
	st.Clock.Advance(120)
	if free := st.NameReservedUntil(ws.ID, "Notebook", "expiring", time.Minute); !free.IsZero() {
		t.Errorf("still held two minutes past a one-minute window: %v", free)
	}
	// And the create succeeds again, which is the behaviour a caller sees.
	if w := do(a.createItem, admin, "POST",
		`{"displayName":"expiring","type":"Notebook"}`, map[string]string{"wid": ws.ID}); w.Code >= 300 {
		t.Errorf("create after expiry = %d %s", w.Code, w.Body.Bytes())
	}
}

// A display name containing a quote must not be able to forge the message's
// own structure. The tenant's wording wraps the name in quotes, so anything
// interpolated there is inside a quoted context — and a client that parses the
// name back out of the prose is the thing being protected.
func TestAQuotedDisplayNameCannotForgeTheMessage(t *testing.T) {
	a, st := newAPI(t)
	a.NameReservation = time.Hour
	ws := seedWorkspace(t, st)
	evil := `it's "quoted" and \ escaped`
	it := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: evil}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteItem(ws.ID, it.ID); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"displayName": evil, "type": "Notebook"})
	w := do(a.createItem, admin, "POST", string(body), map[string]string{"wid": ws.ID})
	if w.Code != http.StatusConflict {
		t.Fatalf("reserved name = %d %s", w.Code, w.Body.Bytes())
	}
	// The response is still well-formed JSON carrying the real fields — the
	// property that matters, and the one a hand-built payload would break.
	var got struct {
		ErrorCode   string `json:"errorCode"`
		Message     string `json:"message"`
		IsRetriable *bool  `json:"isRetriable"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v — %s", err, w.Body.Bytes())
	}
	if got.ErrorCode != "ItemDisplayNameNotAvailableYet" || got.IsRetriable == nil || !*got.IsRetriable {
		t.Errorf("fields lost: %+v", got)
	}
	// And the name appears escaped rather than raw, so the quotes in it cannot
	// close the ones the message opened.
	if !strings.Contains(got.Message, `\"quoted\"`) {
		t.Errorf("name not escaped in message: %q", got.Message)
	}
}
