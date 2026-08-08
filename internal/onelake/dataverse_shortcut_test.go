package onelake

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Dataverse shortcuts, in the data plane.
//
// The scope here is deliberately narrow and worth stating so the tests are not
// read as more than they are: this is a Dataverse *target type* in the
// shortcut machinery that already carried ADLS Gen2 and Amazon S3. It is not a
// Dataverse emulator. Microsoft documents the shortcut target's FIELDS —
// connectionId, deltaLakeFolder, environmentDomain, tableName — and documents
// that the tables must already exist in the Dataverse Managed Lake. It does
// not document the byte layout of that lake. So the emulator serves ordinary
// Delta from `environmentDomain/deltaLakeFolder/tableName`, and that
// composition is OURS, not Microsoft's.
//
// What IS load-bearing and documented is the read-only rule, quoted twice on
// the same page: "Dataverse shortcuts are read-only. They don't support write
// operations regardless of the user's permissions."

// serviceWithItem returns a Service over an open store plus a lakehouse to
// hang shortcuts on. resolveRead needs a real item: it consults the local path
// table first and only then falls through to the shortcut.
func serviceWithItem(t *testing.T) (*Service, string) {
	t.Helper()
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ws := &store.Workspace{DisplayName: "dv"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "owner", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	return New(st, nil), lake.ID
}

// basicConnection is the emulator's own connection model standing in for
// Dataverse's delegated authorization. Fabric documents the supported
// delegated credential types as organizational account (OAuth2) and service
// principal — NOT username/password — so this is a divergence, disclosed on
// the parity row rather than papered over. What the tests here can honestly
// prove is that the shortcut's credential is fetched, applied, and that a
// wrong one fails the read; which credential shape travels is the emulator's.
func basicConnection(t *testing.T, s *Service, user, pass string) *store.Connection {
	t.Helper()
	conn := &store.Connection{
		DisplayName:     "dataverse",
		CredentialsJSON: `{"credentialType":"Basic","username":"` + user + `","password":"` + pass + `"}`,
	}
	if err := s.Store.CreateConnection(conn); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	return conn
}

// dataverseShortcut builds the store row the API layer would have written.
func dataverseShortcut(t *testing.T, s *Service, itemID, endpoint, connID string) *store.Shortcut {
	t.Helper()
	sc := &store.Shortcut{
		ItemID: itemID, Path: "Tables", Name: "account",
		TargetType:     "Dataverse",
		TargetLocation: endpoint,
		TargetPath:     "deltalake",
		TargetTable:    "account",
		ConnectionID:   connID,
	}
	if err := s.Store.CreateShortcut(sc); err != nil {
		t.Fatalf("CreateShortcut: %v", err)
	}
	return sc
}

// A read through the shortcut must reach the target at the composed path, and
// return the target's bytes rather than anything of ours.
func TestDataverseShortcutReadsThroughToTheTarget(t *testing.T) {
	const payload = "id,name\n1,Contoso\n"
	var gotPath string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(payload))
	}))
	defer target.Close()

	s, itemID := serviceWithItem(t)
	conn := basicConnection(t, s, "dv-user", "dv-secret")
	dataverseShortcut(t, s, itemID, target.URL, conn.ID)

	p, derr := s.resolveRead(itemID, "Tables/account/part-0000.parquet", "")
	if derr != nil {
		t.Fatalf("read through the Dataverse shortcut failed: %+v", derr)
	}
	if string(p.Content) != payload {
		t.Fatalf("content = %q, want the target's bytes %q", p.Content, payload)
	}
	// deltaLakeFolder and tableName are separate documented fields, so the
	// composed request must contain BOTH plus the remainder — a target that
	// dropped tableName would still 200 here against a permissive stub, which
	// is why the path is asserted and not just the body.
	if want := "/deltalake/account/part-0000.parquet"; gotPath != want {
		t.Fatalf("upstream path = %q, want %q (environmentDomain + deltaLakeFolder + tableName + remainder)", gotPath, want)
	}
}

// THE DOCUMENTED RULE. A write must be refused, and refused where the caller
// can see it — not absorbed into a local buffer that later reads serve back.
func TestDataverseShortcutRefusesWrites(t *testing.T) {
	var upstream int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			upstream++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	s, itemID := serviceWithItem(t)
	conn := basicConnection(t, s, "dv-user", "dv-secret")
	sc := dataverseShortcut(t, s, itemID, target.URL, conn.ID)

	derr := s.writeExternal(sc, "part-0000.parquet", []byte("nope"))
	if derr == nil {
		t.Fatal("a write through a Dataverse shortcut was accepted; the reference says " +
			"they are read-only \"regardless of the user's permissions\"")
	}
	if derr.status != http.StatusBadRequest || derr.code != "UnsupportedOperation" {
		t.Fatalf("refusal = %d/%s, want 400/UnsupportedOperation (the same refusal S3 gets)", derr.status, derr.code)
	}
	if upstream != 0 {
		t.Fatalf("%d non-GET request(s) reached the target; a refused write must not be forwarded", upstream)
	}
}

// Delete is the other half of the same rule, and it had its own call site.
func TestDataverseShortcutRefusesDeletes(t *testing.T) {
	var upstream int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			upstream++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	s, itemID := serviceWithItem(t)
	conn := basicConnection(t, s, "dv-user", "dv-secret")
	sc := dataverseShortcut(t, s, itemID, target.URL, conn.ID)

	derr := s.deleteExternal(sc, "part-0000.parquet")
	if derr == nil {
		t.Fatal("a delete through a Dataverse shortcut was accepted")
	}
	if derr.status != http.StatusBadRequest || derr.code != "UnsupportedOperation" {
		t.Fatalf("refusal = %d/%s, want 400/UnsupportedOperation", derr.status, derr.code)
	}
	if upstream != 0 {
		t.Fatalf("%d DELETE(s) reached the target; a refused delete must not be forwarded", upstream)
	}
}

// The two predicates are distinct SETS, and collapsing them is the mistake
// that would let a read-only target accept a write.
//
// This is a map-level test and nothing more: it proves the predicates classify
// correctly, NOT that any call site consults them. That gap is real — it is
// the shape that let Spark Job Definitions sit at 🟡 with green unit tests and
// no dispatch — so the route-level half lives in the test below, and neither
// stands in for the other.
func TestExternalAndWritableAreDistinctSets(t *testing.T) {
	// Driven from store's registry, not a second copy of it — a hand-written
	// list here would go green exactly when the two drifted apart.
	for _, typeName := range store.ExternalTargetTypes() {
		sc := &store.Shortcut{TargetType: typeName}
		if !sc.IsExternalTarget() {
			t.Errorf("IsExternalTarget(%s) = false — reads would resolve locally and "+
				"writes would be buffered here instead of refused or forwarded", typeName)
		}
	}
	// OneLake is internal: it must NOT take the external path, or an in-tenant
	// shortcut would try to resolve over HTTP against an empty location.
	if (&store.Shortcut{TargetType: "OneLake"}).IsExternalTarget() {
		t.Error("externalTarget(OneLake) = true; OneLake targets resolve in-store")
	}
	// And writable is a strictly narrower set than external — the two must not
	// collapse into one predicate, because that is what would let a read-only
	// target accept a write.
	for _, typeName := range []string{"AmazonS3", "Dataverse"} {
		if externalWritable(&store.Shortcut{TargetType: typeName}) {
			t.Errorf("externalWritable(%s) = true, but the reference documents it read-only", typeName)
		}
	}
	if !externalWritable(&store.Shortcut{TargetType: "ADLSGen2"}) {
		t.Error("externalWritable(ADLSGen2) = false; ADLS Gen2 carries no read-only restriction")
	}
}

// The credential really is presented upstream, and a target that refuses it
// fails the read rather than returning empty content. Without this a broken
// credential path would look identical to a successful read of an empty file.
func TestDataverseShortcutSurfacesAnAuthFailure(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if user != "dv-user" || pass != "dv-secret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()

	s, itemID := serviceWithItem(t)
	wrong := basicConnection(t, s, "dv-user", "wrong-secret")
	dataverseShortcut(t, s, itemID, target.URL, wrong.ID)

	_, derr := s.resolveRead(itemID, "Tables/account/part-0000.parquet", "")
	if derr == nil {
		t.Fatal("a read with the wrong credential succeeded; the credential is not reaching the target")
	}
	if derr.status != http.StatusNotFound && derr.status != http.StatusBadGateway {
		t.Fatalf("refused read status = %d (%s), want the upstream refusal surfaced", derr.status, derr.code)
	}
}

// An unsupported credential type must fail loudly at the point of use rather
// than silently sending an unauthenticated request.
func TestDataverseShortcutRefusesAnUnsupportedCredentialType(t *testing.T) {
	s, itemID := serviceWithItem(t)
	conn := &store.Connection{
		ID: "dv-oauth", DisplayName: "dv", ConnectivityType: "ShareableCloud",
		CredentialsJSON: `{"credentialType":"OAuth2"}`,
	}
	if err := s.Store.CreateConnection(conn); err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	dataverseShortcut(t, s, itemID, "http://127.0.0.1:1", conn.ID)

	_, derr := s.resolveRead(itemID, "Tables/account/part-0000.parquet", "")
	if derr == nil {
		t.Fatal("a shortcut with an unusable credential resolved anyway")
	}
	if !strings.Contains(derr.code, "Credential") {
		t.Fatalf("error code = %q, want the credential to be named — an unsupported "+
			"credential must not degrade to an anonymous request", derr.code)
	}
}

// THE ROUTE-LEVEL HALF, and the reason externalTarget() exists.
//
// Everything above calls writeExternal/deleteExternal directly, which proves
// the refusal works when it is REACHED. It cannot prove the DFS handler
// reaches it. Before the predicate, flush and delete each carried their own
// literal `ADLSGen2 || AmazonS3` list, so a Dataverse shortcut added to the
// read list alone would have taken neither branch: the flush would store the
// bytes in the LOCAL path table, answer 200, and a later read would serve them
// back as if the target held them. Nothing would have failed.
//
// So this drives the real HTTP surface — PUT, append, flush, DELETE — against
// a Dataverse shortcut and asserts the refusal arrives there. Re-inline a type
// list at either call site and this test goes red; the predicate test above
// would not.
func TestTheDFSSurfaceRefusesWritesThroughADataverseShortcut(t *testing.T) {
	var upstream []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream = append(upstream, r.Method)
		_, _ = w.Write([]byte("target-bytes"))
	}))
	defer target.Close()

	f := newFixture(t)
	conn := &store.Connection{DisplayName: "dv", CredentialsJSON: `{"credentialType":"Anonymous"}`}
	if err := f.st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	sc := &store.Shortcut{
		ItemID: f.it.ID, Path: "Tables", Name: "account",
		TargetType: "Dataverse", TargetLocation: target.URL,
		TargetPath: "deltalake", TargetTable: "account", ConnectionID: conn.ID,
	}
	if err := f.st.CreateShortcut(sc); err != nil {
		t.Fatal(err)
	}
	base := "/" + f.ws.ID + "/" + f.it.ID + "/Tables/account/part-0000.parquet"

	// The read must work, or the refusals below prove nothing about routing.
	if w := f.do("GET", base, f.token, nil); w.Code != http.StatusOK || w.Body.String() != "target-bytes" {
		t.Fatalf("read through the shortcut = %d %q, want 200 and the target's bytes", w.Code, w.Body)
	}

	// Now the write. The flush is the operation that decides where bytes go.
	f.do("PUT", base+"?resource=file", f.token, nil)
	f.do("PATCH", base+"?action=append&position=0", f.token, []byte("local"))
	w := f.do("PATCH", base+"?action=flush&position=5", f.token, nil)
	if w.Code == http.StatusOK {
		t.Fatal("flush through a read-only Dataverse shortcut returned 200 — the bytes " +
			"were buffered locally and the caller was told the write succeeded")
	}
	if got := errCode(t, w); got != "UnsupportedOperation" {
		t.Fatalf("flush error code = %q, want UnsupportedOperation", got)
	}

	// And DELETE, the second call site, which had its own copy of the list.
	//
	// The assertion is the ERROR CODE, not merely "not 200". Asserting non-200
	// here passes against the un-fixed code: with the flush already refused
	// there is no local path, so an unrecognised external type falls through to
	// the local delete and 404s. Two different failures, one indistinguishable
	// status — and the weaker assertion let the mutation survive when it was
	// first written. UnsupportedOperation is reachable only via deleteExternal.
	w = f.do("DELETE", base, f.token, nil)
	if w.Code == http.StatusOK {
		t.Fatal("DELETE through a read-only Dataverse shortcut succeeded")
	}
	if got := errCode(t, w); got != "UnsupportedOperation" {
		t.Fatalf("DELETE error code = %q, want UnsupportedOperation — a %q means the "+
			"handler never recognised the shortcut as external and fell through "+
			"to the local path table", got, got)
	}

	// Whatever the emulator did, it must not have been forwarded upstream.
	for _, method := range upstream {
		if method != http.MethodGet {
			t.Fatalf("upstream saw a %s; a read-only target must receive reads only", method)
		}
	}
}
