package api

import (
	"strings"
	"testing"
)

// These pin the SHAPES a real client refuses, each measured in e2e/sempy. Every
// one of them first failed with a message naming something OTHER than its
// cause, so each test is named for the cause rather than the symptom.

func TestRoutingReplyIsABareArrayWithAnEnumSkuAndAPathSegment(t *testing.T) {
	// An envelope fails as "The specified Power BI workspace is not found";
	// "P1" as "Requested value 'P1' was not found"; a bare origin as
	// IndexOutOfRangeException.
	a, _ := newAPI(t)
	w := do(a.xmlaRouting, admin, "GET", "", nil)
	body := w.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Fatalf("routing reply must be a BARE ARRAY, got %.80s", body)
	}
	if !strings.Contains(body, `"capacitySku":"Premium"`) {
		t.Fatalf("capacitySku must be the enum value Premium: %.200s", body)
	}
	uri := betweenTags(body, `"capacityUri":"`, `"`)
	trimmed := strings.TrimPrefix(strings.TrimPrefix(uri, "https://"), "http://")
	if !strings.Contains(trimmed, "/") {
		t.Fatalf("capacityUri needs at least one path segment, got %q", uri)
	}
}

func TestClusterResolveReturnsABareFqdnUnderTheReadContract(t *testing.T) {
	// Consumed as UriBuilder{Host = clusterFQDN}; a scheme or port throws
	// UriFormatException. The sibling contract (FixedClusterUri) leaves Host nil.
	a, _ := newAPI(t)
	w := do(a.xmlaClusterResolve, admin, "POST", "{}", nil)
	body := w.Body.String()
	if !strings.Contains(body, `"clusterFQDN"`) {
		t.Fatalf("must answer with NameResolutionResult.clusterFQDN: %s", body)
	}
	fqdn := betweenTags(body, `"clusterFQDN":"`, `"`)
	if fqdn == "" || strings.Contains(fqdn, "://") || strings.Contains(fqdn, ":") {
		t.Fatalf("clusterFQDN must be a BARE FQDN, got %q", fqdn)
	}
}

func TestGetDatabaseNameEchoesTheDatasetTheClientAskedFor(t *testing.T) {
	// Only a REAL client issues this: a probe connects with a name it knows.
	a, _ := newAPI(t)
	w := do(a.xmlaDatabaseName, admin, "POST",
		`{"datasetName":"sales","workspaceType":1}`, nil)
	if !strings.Contains(w.Body.String(), `"databaseName":"sales"`) {
		t.Fatalf("got %s", w.Body.String())
	}
}

func TestTheHandshakeCarriesTheCapsHeaderSessionAndTrailingByte(t *testing.T) {
	// The caps header is checked BEFORE the body is read, so its absence
	// discards a perfectly good envelope. The trailing 0x00 is the payload
	// reader's completion marker; any other last byte is a
	// TransportProtocolError.
	a, _ := newAPI(t)
	w := do(a.xmlaEndpoint, admin, "POST",
		`<Envelope><Body><Execute><Command><Statement/></Command></Execute></Body></Envelope>`,
		nil)
	if w.Header().Get("x-ms-xmlacaps-negotiation-flags") == "" {
		t.Fatal("response is missing x-ms-xmlacaps-negotiation-flags")
	}
	b := w.Body.Bytes()
	if len(b) == 0 || b[len(b)-1] != 0x00 {
		t.Fatal("XMLA payload must end with the 0x00 completion byte")
	}
	if !strings.Contains(string(b), "SessionId=") {
		t.Fatal("the handshake must return a Session header")
	}
}

func TestAsslRootFollowsTheRestrictionAndCarriesCompatibilityLevel(t *testing.T) {
	// <DatabaseID> present => Tabular.Database, absent => Tabular.Server; the
	// wrong root is reported as "Unexpected root 'Server' ... when trying to
	// read '...Database'". CompatibilityLevel lives in nsCompat (not the 2003
	// engine namespace, not .../200/200) and <Model> is XmlIgnore on the type.
	srv := asslDocument(false, "m")
	db := asslDocument(true, "m")
	if !strings.HasPrefix(srv, "<Server") {
		t.Fatalf("server-scoped ASSL must be rooted at <Server>: %.30s", srv)
	}
	if !strings.HasPrefix(db, "<Database") {
		t.Fatalf("database-scoped ASSL must be rooted at <Database>: %.30s", db)
	}
	if !strings.Contains(db, nsCompat) || !strings.Contains(db, "CompatibilityLevel") {
		t.Fatal("CompatibilityLevel must be present, in the ddl200 namespace")
	}
	if strings.Contains(db, "<Model>") {
		t.Fatal("<Model> is XmlIgnore on Tabular.Database and must not appear")
	}
}

func TestABatchIsDetectedBeforeAPlainDiscover(t *testing.T) {
	// A batched Execute CONTAINS <Discover> children, so testing for "<Discover"
	// first shadows the batch entirely — a bug this harness shipped twice.
	batch := `<Execute><Command><Batch>` +
		`<Discover><RequestType>TMSCHEMA_MODEL</RequestType></Discover>` +
		`<Discover><RequestType>TMSCHEMA_TABLES</RequestType></Discover>` +
		`</Batch></Command></Execute>`
	if got := len(reRequestType.FindAllStringSubmatch(batch, -1)); got != 2 {
		t.Fatalf("expected 2 request types in the batch, got %d", got)
	}
	if !strings.Contains(batch, "<Batch") {
		t.Fatal("the batch discriminator must be <Batch, not <Discover")
	}
}
