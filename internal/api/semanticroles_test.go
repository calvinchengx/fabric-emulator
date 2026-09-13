package api

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Stage 1 of semantic-model roles: a model's roles are not evaluated yet, so a
// principal they apply to is REFUSED rather than served every row. Each refusal
// is paired with a Write holder reading the same model in the same run, since
// the product exempts them and a refusal of everyone would pass alone.

const salesQueryRetail = `{"queries":[{"query":"EVALUATE 'Store'"}]}`

// securedRetail is the retail fixture with one role added: Viewers in it see a
// single territory. Its content is irrelevant to stage 1 — only its existence.
func securedRetail(t *testing.T, st *store.Store, wsID string) *store.Item {
	t.Helper()
	bim := smFixture(t, "retail.bim")
	roles := []byte(`"roles":[{"name":"One territory","modelPermission":"read",
	  "members":[{"memberName":"viewer@contoso.com","memberId":"viewer-1","identityProvider":"AzureAD"}],
	  "tablePermissions":[{"name":"Store","filterExpression":"'Store'[Territory] = \"NC\""}]}],`)
	i := bytes.Index(bim, []byte(`"tables"`))
	if i < 0 {
		t.Fatal("fixture has no tables key to insert roles before")
	}
	secured := append(append(append([]byte{}, bim[:i]...), roles...), bim[i:]...)
	part := func(path string, data []byte) store.DefinitionPart {
		return store.DefinitionPart{Path: path, PayloadType: "InlineBase64", Payload: base64.StdEncoding.EncodeToString(data)}
	}
	it := &store.Item{WorkspaceID: wsID, Type: "SemanticModel", DisplayName: "SecuredRetail"}
	if err := st.CreateItem(it, []store.DefinitionPart{part("model.bim", secured), part("data.json", smFixture(t, "seed_data.json"))}); err != nil {
		t.Fatal(err)
	}
	return it
}

func TestAModelsRolesRefuseThePrincipalsTheyApplyTo(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	assignRole(t, st, ws.ID, contributor, store.RoleContributor)
	model := securedRetail(t, st, ws.ID)
	pv := map[string]string{"datasetId": model.ID}
	grantBuild(t, st, model, viewer.ID)
	grantBuild(t, st, model, stranger.ID)

	// A Viewer with Build, and a principal with no role holding Read and Build:
	// both are subject to the roles, so both are refused, by name.
	for _, p := range []*auth.Principal{viewer, stranger} {
		w := do(a.executeQueries, p, "POST", salesQueryRetail, pv)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "defines security roles") {
			t.Errorf("%s = %d %s, want the roles refusal", p.ID, w.Code, w.Body)
		}
	}
	// Write holders are exempt, as the product documents — same query, same run.
	for _, p := range []*auth.Principal{admin, contributor} {
		if w := do(a.executeQueries, p, "POST", salesQueryRetail, pv); w.Code != http.StatusOK {
			t.Errorf("%s (Write) = %d %s, want the rows", p.ID, w.Code, w.Body)
		}
	}
}

// The refusal sits in the loader XMLA uses too, so a Viewer cannot reach the
// rows by changing protocol.
func TestTheRolesRefusalCoversTheXMLALoader(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	model := securedRetail(t, st, ws.ID)
	if _, _, err := a.loadSemanticModel(t.Context(), model.ID, viewer); !errors.Is(err, errRolesNotApplied) {
		t.Fatalf("viewer load = %v, want the roles refusal", err)
	}
	if _, _, err := a.loadSemanticModel(t.Context(), model.ID, admin); err != nil {
		t.Fatalf("owner load = %v", err)
	}
}

// A TMDL model's roles refuse the same way: TMDL used to skip `role` blocks.
func TestATMDLModelsRolesRefuseToo(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	tmdl := func(path, text string) store.DefinitionPart {
		return store.DefinitionPart{Path: path, PayloadType: "InlineBase64", Payload: base64.StdEncoding.EncodeToString([]byte(text))}
	}
	it := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "TmdlSecured"}
	if err := st.CreateItem(it, []store.DefinitionPart{
		tmdl("definition/tables/Store.tmdl", "table Store\n\tcolumn Territory\n\t\tdataType: string\n"),
		tmdl("definition/roles/R.tmdl", "role R\n\tmodelPermission: read\n\ttablePermission Store = 'Store'[Territory] = \"NC\"\n"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.loadSemanticModel(t.Context(), it.ID, viewer); !errors.Is(err, errRolesNotApplied) {
		t.Fatalf("viewer load = %v, want the roles refusal", err)
	}
}

// The portal runs as nobody, so it refuses a secured model outright — and still
// serves one without roles.
func TestThePortalRunnerRefusesASecuredModel(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	secured := securedRetail(t, st, ws.ID)
	plain := createSemanticModel(t, st, ws.ID)
	if _, err := a.QueryModelUnauthenticated(secured.ID, "EVALUATE 'Store'"); err == nil ||
		!strings.Contains(err.Error(), "security roles") {
		t.Fatalf("secured = %v, want a refusal naming the roles", err)
	}
	if _, err := a.QueryModelUnauthenticated(plain.ID, "EVALUATE 'Store'"); err != nil {
		t.Fatalf("plain = %v", err)
	}
}

// Deciding whether the roles apply reads the store; a failure is a refusal,
// never an admission.
func TestTheRolesCheckFailsClosed(t *testing.T) {
	t.Run("the caller's access", func(t *testing.T) {
		a, st, dir := newDiskAPI(t)
		ws := seedWorkspace(t, st)
		model := securedRetail(t, st, ws.ID)
		dropTable(t, dir, "role_assignments")
		if _, _, err := a.loadSemanticModel(t.Context(), model.ID, admin); err == nil {
			t.Fatal("an unreadable role was treated as Write")
		}
	})
	t.Run("the item", func(t *testing.T) {
		a, st := newAPI(t)
		ws := seedWorkspace(t, st)
		model := securedRetail(t, st, ws.ID)
		m, err := a.parseModelDefinition(model.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.refuseUnappliedRoles("no-such-item", m, admin); err == nil {
			t.Fatal("a secured model whose item cannot be read was admitted")
		}
	})
}
