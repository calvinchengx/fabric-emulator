package api

// The XMLA REQUEST PATH, driven through the handlers themselves rather than
// through a real ADOMD.NET client.
//
// WHY THIS FILE EXISTS. Every shape below was already measured against
// Microsoft's own clients and is recorded in xmla.go's doc comments — but the
// only thing that EXECUTED any of it was e2e/sempy and e2e/xmla, which are
// weekly (`cron: * * 3`) and need docker. So a regression in Discover, the TOM
// batch, Execute, SaveChanges, the ack, the fault or model resolution shipped
// green for up to seven days. scripts/check_witnesses.py states the matching
// principle in its own words: a witness whose NAME exists is not a witness that
// RAN. These run offline in the per-push suite, with no engine and no container.
//
// They are OUR OWN CLIENT, deliberately. They pin the contracts the comments
// beside the code already record; they are not third-party evidence and must
// never be promoted into a parity witness (docs/24's XMLA row stays `ci:sempy`).
//
// Each test is named for the CAUSE a real client reported, matching
// xmla_test.go's convention, and each assertion carries the client-side failure
// it pins — read off xmla.go rather than invented here.

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// doXMLA drives the endpoint with request HEADERS, which `do` cannot set.
//
// Not a duplicate of `do` for its own sake: the whole model-resolution path
// (xmlaItemID, xmlaItem, xmlaDatabaseIdentity) reads `x-ms-xmlaserver` off the
// request, so a helper that cannot set a header cannot reach the by-name branch
// at all — it would only ever exercise the fallback.
func doXMLA(t *testing.T, a *API, p *auth.Principal, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/webapi/xmla", strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	a.xmlaEndpoint(w, r, p)
	return w
}

// xmlaFixture seeds a workspace with the retail semantic model, so every test
// below runs against the same model the DAX goldens use.
func xmlaFixture(t *testing.T) (*API, *store.Store, *store.Item) {
	t.Helper()
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	return a, st, createSemanticModel(t, st, ws.ID)
}

// discoverEnvelope is a standalone Discover: exactly one <RequestType>, no
// <Batch>, which is the ladder's len(types)==1 arm.
func discoverEnvelope(requestType, restrictions string) string {
	return `<Envelope><Body><Discover xmlns="urn:schemas-microsoft-com:xml-analysis">` +
		`<RequestType>` + requestType + `</RequestType>` +
		`<Restrictions><RestrictionList>` + restrictions +
		`</RestrictionList></Restrictions></Discover></Body></Envelope>`
}

// batchEnvelope is TOM's catalogue request: one Execute whose Command is a
// <Batch> of <Discover> elements.
func batchEnvelope(requestTypes ...string) string {
	var b strings.Builder
	b.WriteString(`<Envelope><Body><Execute xmlns="urn:schemas-microsoft-com:xml-analysis">` +
		`<Command><Batch>`)
	for _, rt := range requestTypes {
		b.WriteString(`<Discover><RequestType>` + rt + `</RequestType></Discover>`)
	}
	b.WriteString(`</Batch></Command></Execute></Body></Envelope>`)
	return b.String()
}

// statementEnvelope is the Execute both grammars arrive by — a DMV SELECT and a
// DAX EVALUATE differ only in the text between the tags.
func statementEnvelope(stmt string) string {
	return `<Envelope><Body><Execute xmlns="urn:schemas-microsoft-com:xml-analysis">` +
		`<Command><Statement>` + stmt + `</Statement></Command></Execute></Body></Envelope>`
}

// writeEnvelope is TOM's SaveChanges: a <Batch Transaction="true"> of
// <Create>/<Alter>, carrying NO <RequestType> — which is what distinguishes it
// from the Discover batch in the dispatch ladder.
func writeEnvelope(inner string) string {
	return `<Envelope><Body><Execute xmlns="urn:schemas-microsoft-com:xml-analysis">` +
		`<Command><Batch Transaction="true" ` +
		`xmlns="http://schemas.microsoft.com/analysisservices/2003/engine">` +
		inner + `</Batch></Command></Execute></Body></Envelope>`
}

// xmlaBody is the response payload with its trailing protocol byte removed, so
// a test can parse it as XML. The byte is asserted here rather than skipped: it
// is not whitespace, so leaving it on would make every parse below fail for a
// reason that is not the parse.
func xmlaBody(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	b := w.Body.Bytes()
	if len(b) == 0 || b[len(b)-1] != 0x00 {
		t.Fatalf("XMLA payload must end with the 0x00 completion byte; any other "+
			"last byte is a TransportProtocolError. got %q", lastByte(b))
	}
	return string(b[:len(b)-1])
}

func lastByte(b []byte) string {
	if len(b) == 0 {
		return "<empty body>"
	}
	return string(b[len(b)-1:])
}

// mustBeWellFormed parses the payload. Every shape here is read by a real XML
// deserialiser before any of its content is looked at, so a malformed envelope
// fails at the client with a message about the parser rather than about us.
func mustBeWellFormed(t *testing.T, payload string) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(payload))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("response is not well-formed XML: %v\n%.400s", err, payload)
		}
	}
}

// TestTheDispatchLadderIsOrderedSoABatchIsNeverShadowed walks every arm of
// xmlaEndpoint's switch, asserting which one ran from the RESPONSE SHAPE rather
// than from internals.
//
// The ORDER is load-bearing and the file records shipping the bug twice: a
// batched Execute CONTAINS <Discover> children, so testing for "<Discover"
// first shadows the batch entirely.
func TestTheDispatchLadderIsOrderedSoABatchIsNeverShadowed(t *testing.T) {
	a, _, _ := xmlaFixture(t)

	const multipleResults = `<results xmlns="` + nsMultipleResults + `">`

	for _, tc := range []struct {
		name    string
		body    string
		want    []string
		notWant []string
	}{{
		// The session handshake: an Execute with an EMPTY <Statement/>, which
		// matches none of the discriminators and must fall to the default arm.
		name: "an empty statement is the handshake",
		body: `<Envelope><Body><Execute><Command><Statement/></Command></Execute></Body></Envelope>`,
		want: []string{`SessionId="` + xmlaSessionID + `"`,
			`<root xmlns="urn:schemas-microsoft-com:xml-analysis:empty"/>`},
	}, {
		name:    "one RequestType is a standalone Discover",
		body:    discoverEnvelope("TMSCHEMA_TABLES", ""),
		want:    []string{`<DiscoverResponse`, `<root name="Table"`},
		notWant: []string{multipleResults},
	}, {
		// Two RequestTypes INSIDE a <Batch>: the arm that must be tested before
		// the single-Discover one, or the batch is answered as one rowset.
		name: "two RequestTypes in a Batch is the TOM catalogue request",
		body: batchEnvelope("TMSCHEMA_MODEL", "TMSCHEMA_TABLES"),
		want: []string{`<ExecuteResponse`, multipleResults,
			`<root name="Model"`, `<root name="Table"`},
		notWant: []string{`<DiscoverResponse`},
	}, {
		// A <Batch> with Create/Alter and NO RequestType: SaveChanges. It shares
		// the multipleresults container with the Discover batch, so the
		// discriminator is that its rowsets are the unnamed one-column acks.
		name: "a Batch of Create with no RequestType is a write",
		body: writeEnvelope(`<Create><Measures><row><TableID>1003</TableID>` +
			`<Name>LadderMeasure</Name><Expression>1</Expression></row></Measures></Create>`),
		want:    []string{`<ExecuteResponse`, multipleResults, `sql:field="ID"`},
		notWant: []string{`<root name=`, `:empty"`},
	}, {
		name: "a non-empty Statement is an Execute",
		body: statementEnvelope(`SELECT [Name] FROM $SYSTEM.TMSCHEMA_TABLES`),
		want: []string{`<ExecuteResponse`, `<Name>Sales</Name>`},
		// Not the batch container: a single Execute answers one rowset in its
		// own envelope, and the container is what a client reads first.
		notWant: []string{multipleResults, `<DiscoverResponse`},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			w := doXMLA(t, a, admin, tc.body, nil)
			body := xmlaBody(t, w)
			mustBeWellFormed(t, body)
			if strings.Contains(body, "<soap:Fault>") {
				t.Fatalf("dispatched to a fault: %.400s", body)
			}
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q\n%.600s", want, body)
				}
			}
			for _, no := range tc.notWant {
				if strings.Contains(body, no) {
					t.Errorf("the wrong arm ran: response contains %q\n%.600s", no, body)
				}
			}
		})
	}
}

// TestABatchAnswersEveryRequestInOrderAndNeverAColumnlessRowset pins the
// invariant xmla.go warns about hardest.
//
// `AmoDataAdapter.AdjustTableNames` renames the DataSet's tables from the
// <root name="..."> attributes and BAILS OUT ENTIRELY when the name count does
// not match the table count — so a single column-less rowset, which Fill skips,
// breaks naming for ALL the others and `DdlUtil.ObtainModelTable`'s
// `Tables["Model"]` comes back null.
//
// The mix is deliberate: two types the retail fixture is rich in and one it has
// none of, because the empty one is the one that would be tempting to omit.
func TestABatchAnswersEveryRequestInOrderAndNeverAColumnlessRowset(t *testing.T) {
	a, _, _ := xmlaFixture(t)
	types := []string{"TMSCHEMA_TABLES", "TMSCHEMA_HIERARCHIES", "TMSCHEMA_MEASURES"}
	wantNames := []string{"Table", "Hierarchies", "Measure"}

	body := xmlaBody(t, doXMLA(t, a, admin, batchEnvelope(types...), nil))
	mustBeWellFormed(t, body)

	// One <root> per request, no more and no fewer: a short answer IS the count
	// mismatch, which is why a truncated envelope is refused at the door.
	roots := strings.Split(body, `<root `)[1:]
	if len(roots) != len(types) {
		t.Fatalf("got %d <root> elements for %d requests; AdjustTableNames bails "+
			"out entirely on a count mismatch\n%.600s", len(roots), len(types), body)
	}
	for i, root := range roots {
		// IN REQUEST ORDER: the client pairs responses to requests positionally.
		if !strings.HasPrefix(root, `name="`+wantNames[i]+`"`) {
			t.Errorf("rowset %d is not %q: %.80s", i, wantNames[i], root)
		}
		// EVERY rowset carries columns, including the ones this model has none
		// of. An empty rowset whose schema declares no columns yields no
		// DataTable at all.
		if !strings.Contains(root, `sql:field=`) {
			t.Errorf("rowset %d (%s) declares no columns; one column-less rowset "+
				"breaks naming for all the others", i, wantNames[i])
		}
	}
}

// TestTheAsslDocumentIsEmbeddedAsXMLAndRootedByTheRestriction covers the
// RawCells wiring that carries the ASSL payload, which
// TestAsslRootFollowsTheRestriction (xmla_test.go) tests only in isolation.
//
// Escaped rather than embedded, the document deserialises to an empty root and
// the client reports `Unexpected root ” (namespace ”)` — a message about the
// ROOT for a defect in ESCAPING.
func TestTheAsslDocumentIsEmbeddedAsXMLAndRootedByTheRestriction(t *testing.T) {
	a, _, _ := xmlaFixture(t)

	for _, tc := range []struct {
		name, restriction, wantRoot, notRoot string
		// CompatibilityLevel is a property of Tabular.DATABASE, so only the
		// database-scoped document carries it; the server-scoped one must not.
		// Both directions are asserted because either mistake is a
		// deserialisation error at the client, not a missing value.
		wantCompat bool
	}{{
		// <DatabaseID> present => the client is reading a Tabular.Database.
		name: "a DatabaseID restriction means a Database", restriction: `<DatabaseID>x</DatabaseID>`,
		wantRoot: `<Database xmlns="` + nsEngine + `">`, notRoot: `<Server xmlns=`,
		wantCompat: true,
	}, {
		// Absent => a Tabular.Server. The wrong root is reported as
		// "Unexpected root 'Server' ... when trying to read '...Database'".
		name: "no restriction means a Server", restriction: "",
		wantRoot: `<Server xmlns="` + nsEngine + `">`, notRoot: `<Database xmlns=`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			body := xmlaBody(t, doXMLA(t, a, admin,
				discoverEnvelope("DISCOVER_XML_METADATA", tc.restriction), nil))
			mustBeWellFormed(t, body)
			if !strings.Contains(body, `<DiscoverResponse`) {
				t.Fatalf("DISCOVER_XML_METADATA must answer a DiscoverResponse: %.300s", body)
			}
			// EMBEDDED, not entity-escaped.
			if !strings.Contains(body, tc.wantRoot) {
				t.Errorf("ASSL document is not embedded as XML: %.600s", body)
			}
			if strings.Contains(body, `&lt;Database`) || strings.Contains(body, `&lt;Server`) {
				t.Errorf("the ASSL document was escaped; it deserialises to an empty "+
					"root and the client reports `Unexpected root ''`: %.400s", body)
			}
			if strings.Contains(body, tc.notRoot) {
				t.Errorf("wrong ASSL root for this restriction: %.400s", body)
			}
			// CompatibilityLevel must be >= 1200 and live in nsCompat (not the
			// 2003 engine namespace, not .../200/200), or the database is not
			// "tabular" to the client.
			if got := strings.Contains(body, nsCompat); got != tc.wantCompat {
				t.Errorf("CompatibilityLevel in the ddl200 namespace present=%v, want %v: %.400s",
					got, tc.wantCompat, body)
			}
		})
	}
}

// TestAnUnrecognisedRequestTypeIsAFaultNotAnInventedEmptyRowset covers the
// branch every error path in the file funnels into, and which is 0.0% today.
//
// "An unrecognised type is not 'this model has none of those'; say so rather
// than inventing an empty rowset for it" — xmla.go. An invented empty rowset is
// the fabrication-class failure: well formed, never errors, silently wrong.
func TestAnUnrecognisedRequestTypeIsAFaultNotAnInventedEmptyRowset(t *testing.T) {
	a, _, _ := xmlaFixture(t)

	t.Run("standalone Discover", func(t *testing.T) {
		body := xmlaBody(t, doXMLA(t, a, admin, discoverEnvelope("DISCOVER_NONSENSE", ""), nil))
		mustBeWellFormed(t, body)
		if !strings.Contains(body, `<faultcode>XMLAnalysisError</faultcode>`) {
			t.Fatalf("an unrecognised request type must be a soap:Fault: %.400s", body)
		}
		if strings.Contains(body, `<root `) {
			t.Fatalf("answered with a rowset instead of faulting: %.400s", body)
		}
	})

	t.Run("inside a batch", func(t *testing.T) {
		// One good type and one bad: the whole batch faults rather than
		// answering a short list, because a short list IS the count mismatch.
		body := xmlaBody(t, doXMLA(t, a, admin,
			batchEnvelope("TMSCHEMA_TABLES", "TMSCHEMA_NOT_A_THING"), nil))
		mustBeWellFormed(t, body)
		if !strings.Contains(body, `<faultcode>XMLAnalysisError</faultcode>`) {
			t.Fatalf("a bad type in a batch must fault, not answer fewer rowsets: %.400s", body)
		}
	})

	t.Run("the faultstring is escaped", func(t *testing.T) {
		// A message carrying XML metacharacters must not break the envelope: a
		// malformed fault fails at the client's parser, which names the parser
		// rather than the error the server was trying to report.
		body := xmlaBody(t, doXMLA(t, a, admin,
			discoverEnvelope("TMSCHEMA_A&B", ""), nil))
		mustBeWellFormed(t, body)
		if !strings.Contains(body, "&amp;") {
			t.Errorf("the faultstring must be XML-escaped: %.400s", body)
		}
		if strings.Contains(body, "TMSCHEMA_A&B") {
			t.Errorf("an unescaped & reached the envelope: %.400s", body)
		}
	})
}

// TestBothExecuteGrammarsAnswerOnOneRouteAndAFailureFaults.
//
// A DMV `SELECT ... FROM $SYSTEM.TMSCHEMA_*` and a DAX `EVALUATE` arrive by the
// SAME route and differ only in their grammar, which is why both are dispatched
// in xmlaExecute. A statement the evaluator cannot answer must fault rather
// than report an empty success — an empty rowset is indistinguishable at the
// client from a model with no rows.
func TestBothExecuteGrammarsAnswerOnOneRouteAndAFailureFaults(t *testing.T) {
	a, _, _ := xmlaFixture(t)

	t.Run("a DMV names the fixture's tables", func(t *testing.T) {
		// A PROJECTION, not `SELECT *`: internal/xmla/dmv.go refuses a star
		// explicitly ("SELECT * is not supported"), because the client joins
		// DataFrames on the names it asked for.
		body := xmlaBody(t, doXMLA(t, a, admin,
			statementEnvelope(`SELECT [Name] FROM $SYSTEM.TMSCHEMA_TABLES`), nil))
		mustBeWellFormed(t, body)
		for _, table := range []string{"Store", "Time", "Sales"} {
			if !strings.Contains(body, `<Name>`+table+`</Name>`) {
				t.Errorf("DMV rowset is missing %s: %.600s", table, body)
			}
		}
	})

	t.Run("a DAX EVALUATE answers rows", func(t *testing.T) {
		body := xmlaBody(t, doXMLA(t, a, admin, statementEnvelope(`EVALUATE 'Store'`), nil))
		mustBeWellFormed(t, body)
		if !strings.Contains(body, `<ExecuteResponse`) {
			t.Fatalf("a DAX EVALUATE must answer an ExecuteResponse: %.300s", body)
		}
		if !strings.Contains(body, "Store A") || !strings.Contains(body, "Store D") {
			t.Errorf("DAX rowset is missing the fixture's rows: %.600s", body)
		}
	})

	t.Run("a statement that cannot be evaluated faults", func(t *testing.T) {
		body := xmlaBody(t, doXMLA(t, a, admin, statementEnvelope(`EVALUATE 'NoSuchTable'`), nil))
		mustBeWellFormed(t, body)
		if !strings.Contains(body, `<faultcode>XMLAnalysisError</faultcode>`) {
			t.Fatalf("a failed evaluation must fault, not answer an empty rowset: %.400s", body)
		}
	})

	t.Run("an unsupported DMV faults", func(t *testing.T) {
		body := xmlaBody(t, doXMLA(t, a, admin,
			statementEnvelope(`SELECT [Name] FROM $SYSTEM.TMSCHEMA_NOT_A_THING`), nil))
		mustBeWellFormed(t, body)
		if !strings.Contains(body, `<faultcode>XMLAnalysisError</faultcode>`) {
			t.Fatalf("an unsupported DMV must fault: %.400s", body)
		}
	})
}

// definitionParts reads an item's stored definition as path -> payload, so a
// test can assert that a write left the parts it did not name alone.
func definitionParts(t *testing.T, st *store.Store, itemID string) map[string]string {
	t.Helper()
	parts, err := st.GetDefinition(itemID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, p := range parts {
		out[p.Path] = p.Payload
	}
	return out
}

// TestASaveChangesLandsInTheModelAndLeavesEverySiblingPartByteIdentical drives
// xmlaWrite, writeAck and replaceDefinitionPart — all three 0.0% today — end to
// end with no engine.
//
// The fixture's TableIDs follow internal/xmla/discover.go's idBase, so
// Store=1001, Time=1002, Sales=1003.
func TestASaveChangesLandsInTheModelAndLeavesEverySiblingPartByteIdentical(t *testing.T) {
	a, st, ds := xmlaFixture(t)
	before := definitionParts(t, st, ds.ID)

	w := doXMLA(t, a, admin, writeEnvelope(
		`<Create><Measures><row><TableID>1003</TableID><Name>NewMeasure</Name>`+
			`<Expression>1</Expression></row></Measures></Create>`), nil)
	body := xmlaBody(t, w)
	mustBeWellFormed(t, body)
	if strings.Contains(body, "<soap:Fault>") {
		t.Fatalf("the write faulted: %.400s", body)
	}

	// THE ACK IS A ROWSET CONTAINER, not the empty handshake root: the empty
	// root sessionEnvelope sends for a bare command is REFUSED here as
	// `ResponseFormatException: The result set returned by the server is not a
	// rowset`.
	if !strings.Contains(body, `<results xmlns="`+nsMultipleResults+`">`) {
		t.Fatalf("a write must be acked with the multipleresults container: %.400s", body)
	}
	if strings.Contains(body, `:empty"`) {
		t.Fatalf("the empty session root is refused as `not a rowset`: %.400s", body)
	}
	// One rowset per command, exactly as the Discover batch is.
	if n := len(strings.Split(body, `<root `)) - 1; n != 1 {
		t.Fatalf("ack carries %d rowsets for 1 command", n)
	}

	// The edit actually landed. TOM reports SaveChanges as successful whenever
	// the server answers, so an ack proves nothing on its own.
	after := definitionParts(t, st, ds.ID)
	bim, err := base64.StdEncoding.DecodeString(after["model.bim"])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(bim, []byte("NewMeasure")) {
		t.Fatalf("the measure is not in the stored model.bim:\n%s", bim)
	}

	// AND THE SIBLINGS ARE UNTOUCHED, which is the whole reason
	// replaceDefinitionPart exists instead of SetDefinition: a semantic model's
	// definition also carries data.json, and replacing the whole definition to
	// change the model silently drops the table data the DAX evaluator reads.
	if len(after) != len(before) {
		t.Fatalf("definition went from %d parts to %d", len(before), len(after))
	}
	for path, payload := range before {
		if path == "model.bim" {
			continue
		}
		if after[path] != payload {
			t.Errorf("%s was rewritten by a model.bim write; the DAX evaluator "+
				"reads it and would now see different rows", path)
		}
	}

	// A dropped data.json would otherwise be invisible: query the model after
	// the write and the rows must still be there.
	q := xmlaBody(t, doXMLA(t, a, admin, statementEnvelope(`EVALUATE 'Store'`), nil))
	if !strings.Contains(q, "Store A") {
		t.Errorf("the write dropped the table data: %.400s", q)
	}
}

// TestAWriteIsRefusedRatherThanSilentlyDropped.
//
// Both refusals exist for one reason, recorded twice in xmla.go: TOM reports
// SaveChanges as SUCCESSFUL whenever the server answers, so accepting a batch
// and dropping it loses the user's edit while telling them it landed.
func TestAWriteIsRefusedRatherThanSilentlyDropped(t *testing.T) {
	t.Run("a model with no model.bim part", func(t *testing.T) {
		// TMDL-serialised models are read fine and written not at all.
		a, st := newAPI(t)
		ws := seedWorkspace(t, st)
		it := &store.Item{WorkspaceID: ws.ID, Type: "SemanticModel", DisplayName: "TmdlModel"}
		parts := []store.DefinitionPart{{Path: "definition/model.tmdl", PayloadType: "InlineBase64",
			Payload: base64.StdEncoding.EncodeToString([]byte("model Model\n"))}}
		if err := st.CreateItem(it, parts); err != nil {
			t.Fatal(err)
		}
		body := xmlaBody(t, doXMLA(t, a, admin, writeEnvelope(
			`<Create><Measures><row><TableID>1001</TableID><Name>M</Name></row></Measures></Create>`), nil))
		mustBeWellFormed(t, body)
		if !strings.Contains(body, `<faultcode>XMLAnalysisError</faultcode>`) {
			t.Fatalf("a model with no model.bim must fault: %.400s", body)
		}
		if !strings.Contains(body, "model.bim") {
			t.Errorf("the fault must name model.bim, or the client cannot tell "+
				"this model apart from a rejected batch: %.400s", body)
		}
	})

	a, st, ds := xmlaFixture(t)
	for _, tc := range []struct {
		name, inner string
	}{{
		// ParseWriteBatch's own refusal: a batch that is not well-formed XML.
		// Half a transaction read as a whole one is the failure that reads as
		// success at the client.
		name:  "a batch that does not parse",
		inner: `<Create><Measures><row><TableID>1003</TableID>`,
	}, {
		// ApplyWrite's: an object type it cannot represent is an ERROR, not a
		// skip. Hierarchies are exactly that — the model format carries none.
		name: "an object type that cannot be applied",
		inner: `<Create><Hierarchies><row><TableID>1003</TableID>` +
			`<Name>H</Name></row></Hierarchies></Create>`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			before := definitionParts(t, st, ds.ID)
			body := xmlaBody(t, doXMLA(t, a, admin, writeEnvelope(tc.inner), nil))
			mustBeWellFormed(t, body)
			if !strings.Contains(body, `<faultcode>XMLAnalysisError</faultcode>`) {
				t.Fatalf("must fault rather than ack a no-op: %.400s", body)
			}
			// A refused write must also not have half-applied.
			after := definitionParts(t, st, ds.ID)
			for path, payload := range before {
				if after[path] != payload {
					t.Errorf("a refused write still rewrote %s", path)
				}
			}
		})
	}
}

// TestTheConnectionResolvesToANamedDatasetAndNeverToNothing covers xmlaModel,
// xmlaItemID, xmlaItem and xmlaDatabaseIdentity — every handler above depends
// on them and all four are 0.0% today.
//
// A miss is an error rather than an empty model: an XMLA client that
// successfully connects to nothing looks like a model with no tables, which is
// the fabrication-class failure — well formed, never errors, silently wrong.
func TestTheConnectionResolvesToANamedDatasetAndNeverToNothing(t *testing.T) {
	a, st, _ := xmlaFixture(t)
	// A second visible model, so "which one" is a real question.
	ws, err := st.ListWorkspacesFor(admin.ID)
	if err != nil || len(ws) == 0 {
		t.Fatal("expected the seeded workspace")
	}
	second := &store.Item{WorkspaceID: ws[0].ID, Type: "SemanticModel", DisplayName: "SecondModel"}
	if err := st.CreateItem(second, nil); err != nil {
		t.Fatal(err)
	}

	// The DISPLAY NAME is what the ASSL document reports, and TOM matches on it:
	// `DatasetNotFoundException: Dataset 'RetailAnalysis' not found` is what an
	// id here produces.
	nameOf := func(t *testing.T, hdr map[string]string) string {
		t.Helper()
		body := xmlaBody(t, doXMLA(t, a, admin,
			discoverEnvelope("DISCOVER_XML_METADATA", `<DatabaseID>x</DatabaseID>`), hdr))
		mustBeWellFormed(t, body)
		return betweenTags(body, "<Name>", "</Name>")
	}

	if got := nameOf(t, map[string]string{"x-ms-xmlaserver": "SecondModel"}); got != "SecondModel" {
		t.Errorf("the header must select the model it names, got %q", got)
	}
	if got := nameOf(t, map[string]string{"x-ms-xmlaserver": "RetailAnalysis"}); got != "RetailAnalysis" {
		t.Errorf("the header must select the model it names, got %q", got)
	}
	// An unmatched header falls back to the first visible model rather than
	// failing: the header names a WORKSPACE more often than a dataset, so the
	// fallback is right far more often than a refusal would be.
	if got := nameOf(t, map[string]string{"x-ms-xmlaserver": "some-workspace"}); got != "RetailAnalysis" {
		t.Errorf("an unmatched header must fall back to the first visible model, got %q", got)
	}
	if got := nameOf(t, nil); got != "RetailAnalysis" {
		t.Errorf("no header must fall back to the first visible model, got %q", got)
	}

	// A principal who can see none gets a fault, never an empty model.
	body := xmlaBody(t, doXMLA(t, a, nobody, discoverEnvelope("TMSCHEMA_TABLES", ""), nil))
	mustBeWellFormed(t, body)
	if !strings.Contains(body, `<faultcode>XMLAnalysisError</faultcode>`) {
		t.Fatalf("a principal with no visible model must fault: %.400s", body)
	}
	if !strings.Contains(body, "no semantic model is visible") {
		t.Errorf("the fault must say so; a client connected to nothing looks "+
			"like a model with no tables: %.400s", body)
	}
	// Every route through xmlaModel behaves the same way, because all of them
	// resolve the connection before they do anything else.
	for name, b := range map[string]string{
		"batch":   batchEnvelope("TMSCHEMA_TABLES", "TMSCHEMA_MEASURES"),
		"execute": statementEnvelope(`EVALUATE 'Store'`),
		"write": writeEnvelope(`<Create><Measures><row><TableID>1003</TableID>` +
			`<Name>M</Name></row></Measures></Create>`),
	} {
		got := xmlaBody(t, doXMLA(t, a, nobody, b, nil))
		if !strings.Contains(got, `<faultcode>XMLAnalysisError</faultcode>`) {
			t.Errorf("%s with no visible model must fault: %.300s", name, got)
		}
	}
}

// TestGroupsAnswersTheODataEnvelopeWhereRoutingAnswersABareArray.
//
// The two shapes differ DELIBERATELY and the client refuses each in the other's
// shape: `GET /v1.0/myorg/groups` is Power BI's OData envelope, while the ADOMD
// routing call is a bare array (an envelope there fails as
// `The specified Power BI workspace is not found`). Asserted as a CONTRAST,
// because either one alone would still pass if both had drifted to one shape.
func TestGroupsAnswersTheODataEnvelopeWhereRoutingAnswersABareArray(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	groups := do(a.listPBIGroups, admin, "GET", "", nil).Body.String()
	if !strings.HasPrefix(strings.TrimSpace(groups), `{`) ||
		!strings.Contains(groups, `"value":[`) {
		t.Fatalf("groups must be the OData envelope; a bare array makes SemPy's "+
			"list_workspaces fail in its parser: %.300s", groups)
	}
	if !strings.Contains(groups, `"id":"`+ws.ID+`"`) {
		t.Errorf("groups must list the caller's workspace: %.300s", groups)
	}

	routing := do(a.xmlaRouting, admin, "GET", "", nil).Body.String()
	if !strings.HasPrefix(strings.TrimSpace(routing), `[`) {
		t.Fatalf("routing must stay a BARE ARRAY: %.200s", routing)
	}
	if strings.Contains(routing, `"value":`) {
		t.Fatalf("routing must not grow the groups envelope: %.200s", routing)
	}

	// A caller who can see nothing still gets the envelope, not a bare null:
	// `{"value":[]}` is a list of no workspaces; `null` is a parse error.
	empty := do(a.listPBIGroups, nobody, "GET", "", nil).Body.String()
	if !strings.Contains(empty, `"value":[]`) {
		t.Errorf("an empty groups list must still be an envelope: %s", empty)
	}
}

// TestClusterResolveAnswersWithNoPrincipalAtAll.
//
// MEASURED: ADOMD.NET sends clusterResolve with NO Authorization header — it is
// asking which host to talk to, before it has decided what credential that host
// wants. So this is the one deliberately unauthenticated route, and a nil deref
// here would be a crash on exactly the route that cannot be retried with
// credentials.
func TestClusterResolveAnswersWithNoPrincipalAtAll(t *testing.T) {
	a, _ := newAPI(t)
	r := httptest.NewRequest("POST", "/webapi/clusterResolve", strings.NewReader("{}"))
	r.Host = "fabric.example:8443"
	w := httptest.NewRecorder()
	a.xmlaClusterResolveOpen(w, r) // no principal: the open entry point

	if w.Code != http.StatusOK {
		t.Fatalf("unauthenticated clusterResolve = %d; a refusal here reports a "+
			"correct client as `missing bearer token`", w.Code)
	}
	// A BARE FQDN: consumed as UriBuilder{Host = clusterFQDN}, so the port must
	// be stripped — the port is inherited from the Data Source and was never
	// ours to send.
	if fqdn := betweenTags(w.Body.String(), `"clusterFQDN":"`, `"`); fqdn != "fabric.example" {
		t.Errorf("clusterFQDN = %q, want the bare host", fqdn)
	}
	// The nil-principal guard itself: visibleWorkspaces would deref p.ID.
	if got := a.xmlaVisibleWorkspaces(nil); got != nil {
		t.Errorf("xmlaVisibleWorkspaces(nil) = %v; the open route has no principal", got)
	}
}

// TestEveryResponseShapeCarriesTheTransportInvariants.
//
// The caps header is checked in ProcessWebResponse BEFORE the body is read, so
// its absence discards a perfectly good envelope with
// `InvalidDataException: The 'x-ms-xmlacaps-negotiation-flags' header is
// missing`. The trailing 0x00 is the payload reader's completion marker; any
// other last byte is a TransportProtocolError.
//
// EVERY shape, because the fault and the ack are the two that build their bytes
// themselves rather than going through a Rowset — they are exactly the pair a
// change to writeXMLA's siblings would miss.
func TestEveryResponseShapeCarriesTheTransportInvariants(t *testing.T) {
	a, _, _ := xmlaFixture(t)

	for _, tc := range []struct{ name, body string }{
		{"handshake", `<Envelope><Body><Execute><Command><Statement/></Command></Execute></Body></Envelope>`},
		{"discover", discoverEnvelope("TMSCHEMA_TABLES", "")},
		{"assl metadata", discoverEnvelope("DISCOVER_XML_METADATA", `<DatabaseID>x</DatabaseID>`)},
		{"batch", batchEnvelope("TMSCHEMA_MODEL", "TMSCHEMA_TABLES")},
		{"dmv execute", statementEnvelope(`SELECT [Name] FROM $SYSTEM.TMSCHEMA_TABLES`)},
		{"dax execute", statementEnvelope(`EVALUATE 'Store'`)},
		{"write ack", writeEnvelope(`<Create><Measures><row><TableID>1003</TableID>` +
			`<Name>InvariantMeasure</Name><Expression>1</Expression></row></Measures></Create>`)},
		{"fault", discoverEnvelope("DISCOVER_NONSENSE", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doXMLA(t, a, admin, tc.body, nil)
			if w.Code != http.StatusOK {
				t.Errorf("status = %d; XMLA reports its own errors in the envelope", w.Code)
			}
			if got := w.Header().Get("Content-Type"); got != "text/xml; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			if w.Header().Get("x-ms-xmlacaps-negotiation-flags") == "" {
				t.Error("missing x-ms-xmlacaps-negotiation-flags; the client checks " +
					"it BEFORE reading the body and discards the envelope")
			}
			// xmlaBody asserts the trailing 0x00 and fails naming it.
			mustBeWellFormed(t, xmlaBody(t, w))
		})
	}
}

// TestAnOversizedEnvelopeIsRefusedRatherThanParsedAsAFragment.
//
// ReadBounded, not a bare LimitReader: a LimitReader DISCARDS the excess and
// reports success, so an oversized envelope would be parsed as a fragment — a
// truncated Batch would silently answer fewer rowsets than were asked for,
// which is exactly the count mismatch that breaks table naming.
func TestAnOversizedEnvelopeIsRefusedRatherThanParsedAsAFragment(t *testing.T) {
	a, _, _ := xmlaFixture(t)

	// Over the 8 MiB ceiling, and shaped like a batch so a truncating reader
	// would produce a plausible — and short — answer rather than an error.
	var b strings.Builder
	b.WriteString(`<Envelope><Body><Execute><Command><Batch>`)
	for b.Len() <= 8<<20 {
		b.WriteString(`<Discover><RequestType>TMSCHEMA_TABLES</RequestType></Discover>`)
	}
	b.WriteString(`</Batch></Command></Execute></Body></Envelope>`)

	w := doXMLA(t, a, admin, b.String(), nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized envelope = %d, want 413; a truncated Batch answers "+
			"fewer rowsets than were asked for and breaks table naming", w.Code)
	}
	if strings.Contains(w.Body.String(), "ExecuteResponse") {
		t.Fatalf("the oversized body was parsed as a fragment: %.200s", w.Body.String())
	}
}
