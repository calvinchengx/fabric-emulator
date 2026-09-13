package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Semantic-model roles, applied. Every restricted answer is paired with a Write
// holder reading the same model in the same run — the product exempts them, so
// a filter applied to everyone would pass the restricted half alone — and with a
// principal the roles do not admit, who sees nothing.

const (
	storeQuery     = `{"queries":[{"query":"EVALUATE 'Store'"}]}`
	salesRowsQuery = `{"queries":[{"query":"EVALUATE 'Sales'"}]}`
)

// ada is a member by sign-in name only: the role lists no id for her.
var ada = &auth.Principal{ID: "ada-1", Type: "User", UPN: "ada@contoso.com"}

// westRoles admits viewer-1 by id and Ada by UPN (written in another case) to
// the West territory. The retail fixture's West stores are 1 and 4.
const westRoles = `"roles":[{"name":"West","modelPermission":"read",
  "members":[{"memberName":"someone-else@contoso.com","memberId":"viewer-1","identityProvider":"AzureAD"},
             {"memberName":"Ada@Contoso.com","identityProvider":"AzureAD"}],
  "tablePermissions":[{"name":"Store","filterExpression":"'Store'[Territory] = \"West\""}]}],`

// securedRetail is the retail fixture with roles inserted.
func securedRetail(t *testing.T, st *store.Store, wsID string) *store.Item {
	return securedRetailWith(t, st, wsID, westRoles)
}

func securedRetailWith(t *testing.T, st *store.Store, wsID, roles string) *store.Item {
	t.Helper()
	bim := smFixture(t, "retail.bim")
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

// column runs one query and returns a column's values, sorted, as text.
func queryColumn(t *testing.T, a *API, p *auth.Principal, model *store.Item, body, col string) []string {
	t.Helper()
	w := do(a.executeQueries, p, "POST", body, map[string]string{"datasetId": model.ID})
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", p.ID, w.Code, w.Body)
	}
	var out struct {
		Results []struct {
			Tables []struct {
				Rows []map[string]any `json:"rows"`
			} `json:"tables"`
		} `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	vals := []string{}
	for _, r := range out.Results[0].Tables[0].Rows {
		b, _ := json.Marshal(r[col])
		vals = append(vals, string(b))
	}
	sort.Strings(vals)
	return vals
}

func TestARoleFiltersRowsForTheMembersItAdmits(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	assignRole(t, st, ws.ID, contributor, store.RoleContributor)
	model := securedRetail(t, st, ws.ID)
	for _, p := range []*auth.Principal{viewer, stranger, ada} {
		grantBuild(t, st, model, p.ID)
	}

	west := []string{"1", "4"}
	for _, p := range []*auth.Principal{viewer, ada} {
		if got := queryColumn(t, a, p, model, storeQuery, "Store[StoreId]"); !equal(got, west) {
			t.Errorf("%s Store = %v, want the West stores %v", p.ID, got, west)
		}
		// The filter on the dimension reaches the facts through the relationship.
		if got := queryColumn(t, a, p, model, salesRowsQuery, "Sales[StoreId]"); !equal(got, []string{"1", "1", "4", "4"}) {
			t.Errorf("%s Sales = %v, want the West stores' rows", p.ID, got)
		}
	}
	// Read and Build, but in no role: no rows, not every row.
	if got := queryColumn(t, a, stranger, model, storeQuery, "Store[StoreId]"); len(got) != 0 {
		t.Errorf("stranger Store = %v, want none", got)
	}
	// Write holders see everything, in the same run.
	for _, p := range []*auth.Principal{admin, contributor} {
		if got := queryColumn(t, a, p, model, salesRowsQuery, "Sales[StoreId]"); len(got) != 8 {
			t.Errorf("%s (Write) Sales = %v, want all 8 rows", p.ID, got)
		}
	}
}

// Roles are additive: a second role admitting the same member adds its rows.
func TestRolesAreAdditiveOverTheWire(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	model := securedRetailWith(t, st, ws.ID, `"roles":[
	  {"name":"West","members":[{"memberId":"viewer-1"}],
	   "tablePermissions":[{"name":"Store","filterExpression":"[Territory] = \"West\""}]},
	  {"name":"Central","members":[{"memberId":"viewer-1"}],
	   "tablePermissions":[{"name":"Store","filterExpression":"[Territory] IN {\"Central\"}"}]}],`)
	grantBuild(t, st, model, viewer.ID)
	if got := queryColumn(t, a, viewer, model, storeQuery, "Store[StoreId]"); !equal(got, []string{"1", "3", "4"}) {
		t.Fatalf("West ∪ Central = %v", got)
	}
}

// The filter is applied in the loader XMLA uses too, so a Viewer cannot reach
// the rows by changing protocol.
func TestRowSecurityCoversTheXMLALoader(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	model := securedRetail(t, st, ws.ID)
	_, data, err := a.loadSemanticModel(t.Context(), model.ID, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if len(data["Store"]) != 2 || len(data["Sales"]) != 4 {
		t.Fatalf("viewer load: Store %d, Sales %d rows; want 2 and 4", len(data["Store"]), len(data["Sales"]))
	}
	if _, data, err = a.loadSemanticModel(t.Context(), model.ID, admin); err != nil || len(data["Store"]) != 4 {
		t.Fatalf("owner load = %d rows, %v", len(data["Store"]), err)
	}
}

// A TMDL model's roles apply the same way: TMDL used to skip `role` blocks.
func TestATMDLModelsRolesApplyToo(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	tmdl := func(path, text string) store.DefinitionPart {
		return store.DefinitionPart{Path: path, PayloadType: "InlineBase64", Payload: base64.StdEncoding.EncodeToString([]byte(text))}
	}
	it := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "TmdlSecured"}
	if err := st.CreateItem(it, []store.DefinitionPart{
		tmdl("definition/tables/Store.tmdl", "table Store\n\tcolumn Territory\n\t\tdataType: string\n"),
		tmdl("definition/roles/R.tmdl", "role R\n\tmodelPermission: read\n\tmember 'someone@contoso.com' = user\n\t\tmemberId: viewer-1\n"+
			"\ttablePermission Store = 'Store'[Territory] = \"NC\"\n"),
		tmdl("data.json", `{"Store":[{"Territory":"NC"},{"Territory":"SC"}]}`),
	}); err != nil {
		t.Fatal(err)
	}
	_, data, err := a.loadSemanticModel(t.Context(), it.ID, viewer)
	if err != nil {
		t.Fatal(err)
	}
	if len(data["Store"]) != 1 || data["Store"][0]["Territory"] != "NC" {
		t.Fatalf("viewer rows = %v, want NC only", data["Store"])
	}
}

// What the loader cannot apply it refuses, for the restricted principal only.
func TestRowSecurityRefusesWhatItCannotApply(t *testing.T) {
	app := &auth.Principal{ID: "app-1", Type: "ServicePrincipal"}
	for name, tc := range map[string]struct {
		roles string
		who   *auth.Principal
		want  string
	}{
		"a service principal below Write": {westRoles, app, "service principals cannot be role members"},
		"object-level security in the caller's role": {`"roles":[{"name":"Hide","members":[{"memberId":"viewer-1"}],
		  "tablePermissions":[{"name":"Store","columnPermissions":[{"name":"PostalCode","metadataPermission":"none"}]}]}],`,
			viewer, `hides objects in table \"Store\"`},
		"a table hidden in the caller's role": {`"roles":[{"name":"Hide","members":[{"memberId":"viewer-1"}],
		  "tablePermissions":[{"name":"Time","metadataPermission":"None"}]}],`, viewer, "object-level security"},
		"a filter outside the subset": {`"roles":[{"name":"Lookup","members":[{"memberId":"viewer-1"}],
		  "tablePermissions":[{"name":"Store","filterExpression":"LOOKUPVALUE('Store'[Store], 'Store'[StoreId], 1) = \"x\""}]}],`,
			viewer, "LOOKUPVALUE is not supported"},
		"a filter that cannot evaluate": {`"roles":[{"name":"Bad","members":[{"memberId":"viewer-1"}],
		  "tablePermissions":[{"name":"Store","filterExpression":"[Territory] = 1"}]}],`, viewer, "cannot compare"},
	} {
		t.Run(name, func(t *testing.T) {
			a, st := newAPI(t)
			ws := seedWorkspace(t, st)
			model := securedRetailWith(t, st, ws.ID, tc.roles)
			grantBuild(t, st, model, tc.who.ID)
			pv := map[string]string{"datasetId": model.ID}
			if w := do(a.executeQueries, tc.who, "POST", storeQuery, pv); w.Code != http.StatusBadRequest ||
				!strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("%s = %d %s, want a refusal naming %q", tc.who.ID, w.Code, w.Body, tc.want)
			}
			if w := do(a.executeQueries, admin, "POST", storeQuery, pv); w.Code != http.StatusOK {
				t.Errorf("admin (Write) = %d %s", w.Code, w.Body)
			}
		})
	}
}

// Object-level security in a role the caller is NOT in does not concern them.
func TestAnotherRolesObjectSecurityDoesNotRefuseTheCaller(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	model := securedRetailWith(t, st, ws.ID, `"roles":[
	  {"name":"West","members":[{"memberId":"viewer-1"}],"tablePermissions":[{"name":"Store","filterExpression":"[Territory] = \"West\""}]},
	  {"name":"Hide","members":[{"memberId":"someone-else"}],"tablePermissions":[{"name":"Time","metadataPermission":"none"}]}],`)
	grantBuild(t, st, model, viewer.ID)
	if got := queryColumn(t, a, viewer, model, storeQuery, "Store[StoreId]"); !equal(got, []string{"1", "4"}) {
		t.Fatalf("viewer = %v", got)
	}
}

// impersonatedUserName asks for somebody else's view; answering with the
// caller's would be a wrong answer, so it is refused — even for an owner.
func TestImpersonationOnASecuredModelIsRefused(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	secured := securedRetail(t, st, ws.ID)
	plain := createSemanticModel(t, st, ws.ID)
	body := `{"queries":[{"query":"EVALUATE 'Store'"}],"impersonatedUserName":"ada@contoso.com"}`
	if w := do(a.executeQueries, admin, "POST", body, map[string]string{"datasetId": secured.ID}); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "ImpersonationNotSupported") {
		t.Errorf("secured = %d %s, want the impersonation refusal", w.Code, w.Body)
	}
	if w := do(a.executeQueries, admin, "POST", body, map[string]string{"datasetId": plain.ID}); w.Code != http.StatusOK {
		t.Errorf("plain = %d %s: a model without roles is unaffected", w.Code, w.Body)
	}
}

// The msmdsrv catalog carries no roles and is cached per item, so a restricted
// caller is never relayed — and the engine is never contacted for them.
func TestARestrictedCallerIsNotRelayedToTheDAXEngine(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	model := securedRetail(t, st, ws.ID)
	grantBuild(t, st, model, viewer.ID)
	hits := 0
	pump := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(pump.Close)
	a.DAXURL, _ = url.Parse(pump.URL)
	pv := map[string]string{"datasetId": model.ID}
	if w := do(a.executeQueries, viewer, "POST", storeQuery, pv); w.Code != http.StatusNotImplemented ||
		!strings.Contains(w.Body.String(), "RowLevelSecurityNotRelayed") {
		t.Errorf("viewer = %d %s, want the relay refusal", w.Code, w.Body)
	}
	if hits != 0 {
		t.Errorf("the engine was contacted %d time(s) for a restricted caller", hits)
	}
	// A Write holder is relayed as before: the engine is contacted.
	if w := do(a.executeQueries, admin, "POST", storeQuery, pv); w.Code != http.StatusBadGateway || hits == 0 {
		t.Errorf("admin = %d %s with %d hit(s), want the relay attempted", w.Code, w.Body, hits)
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
		if _, err := a.rolesRestrict("no-such-item", m, admin); err == nil {
			t.Fatal("a secured model whose item cannot be read was admitted")
		}
	})
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
