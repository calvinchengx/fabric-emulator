package server_test

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// Schema conformance: the emulator's payloads must carry the field names the
// Fabric REST reference documents — no more, no fewer.
//
// Why this exists. Behavioural tests in this repo are strong: real engines
// compute, real clients drive, negative controls prove the checks are not
// vacuous. Schema claims had no such guard, and drifted repeatedly:
//
//   - connection credentials grew invented `accessKeyId`/`secretAccessKey`
//     fields that exist in no documented CredentialType;
//   - the admin surfaces use three different envelope keys (`value`,
//     `workspaces`, `itemEntities`) that cannot be inferred from each other;
//   - an item's `sensitivityLabel` was modelled as a nested object under
//     `properties` when the reference puts it on the item with only an `id`.
//
// Each was found by reading the reference by hand, after the code shipped.
// This table encodes what the reference says, with the page it came from, and
// fails the build when a payload disagrees.
//
// The *extra*-field check is the load-bearing one. A missing field usually
// breaks a client loudly; an invented field is silently accepted by every
// client and quietly wrong — which is exactly how the credential bug survived
// a passing end-to-end suite.
type schemaCase struct {
	name string
	// source is the reference page the expectations came from. Kept in the
	// test so a future reader can re-derive rather than trust it.
	source string
	method string
	path   string
	body   any
	status int
	// envelope is the documented list key ("" when the payload is the object).
	envelope string
	// required must all be present on each object.
	required []string
	// optional may be present. Anything outside required+optional fails.
	optional []string
}

func TestPayloadsMatchDocumentedSchemas(t *testing.T) {
	f := newFixture(t)

	// Fixtures the cases below address.
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "schema-ws"}, &ws)
	var item struct{ ID string }
	f.call("POST", "/v1/workspaces/"+ws.ID+"/notebooks", f.token,
		map[string]any{"displayName": "schema-nb"}, &item)

	cases := []schemaCase{
		{
			name:     "admin workspaces",
			source:   "rest/api/fabric/admin/workspaces/list-workspaces",
			method:   "GET",
			path:     "/v1/admin/workspaces",
			status:   http.StatusOK,
			envelope: "workspaces",
			required: []string{"id", "name", "type", "state"},
			// capacityId and domainId appear when set; the reference's own
			// samples omit them otherwise.
			optional: []string{"capacityId", "domainId", "tags", "encryption"},
		},
		{
			name:     "admin items",
			source:   "rest/api/fabric/admin/items/list-items",
			method:   "GET",
			path:     "/v1/admin/items",
			status:   http.StatusOK,
			envelope: "itemEntities",
			required: []string{"id", "type", "name", "workspaceId", "state"},
			optional: []string{"description", "capacityId", "creatorPrincipal", "lastUpdatedDate"},
		},
		{
			name:     "tenant settings",
			source:   "rest/api/fabric/admin/tenants/list-tenant-settings",
			method:   "GET",
			path:     "/v1/admin/tenantsettings",
			status:   http.StatusOK,
			envelope: "value",
			required: []string{"settingName", "title", "enabled", "canSpecifySecurityGroups"},
			optional: []string{
				"tenantSettingGroup", "delegateToCapacity", "delegateToDomain",
				"delegateToWorkspace", "enabledSecurityGroups", "excludedSecurityGroups",
				"properties",
			},
		},
		{
			name:     "item (user-facing)",
			source:   "rest/api/fabric/core/items/list-items",
			method:   "GET",
			path:     "/v1/workspaces/" + ws.ID + "/items",
			status:   http.StatusOK,
			envelope: "value",
			required: []string{"id", "displayName", "type", "workspaceId"},
			optional: []string{
				"description", "folderId", "properties", "sensitivityLabel",
				"tags", "defaultIdentity",
			},
		},
		{
			name:     "catalog search",
			source:   "rest/api/fabric/core/catalog/search",
			method:   "POST",
			path:     "/v1/catalog/search",
			body:     map[string]any{"search": "schema"},
			status:   http.StatusOK,
			envelope: "value",
			required: []string{"id", "type", "catalogEntryType", "displayName", "hierarchy"},
			optional: []string{"description"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]json.RawMessage
			resp := f.call(tc.method, tc.path, f.token, tc.body, &payload)
			f.mustStatus(resp, tc.status, tc.name)

			raw, ok := payload[tc.envelope]
			if !ok {
				t.Fatalf("no %q key in the response (keys: %v)\nreference: %s",
					tc.envelope, keysOfRaw(payload), tc.source)
			}
			var objects []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &objects); err != nil {
				t.Fatalf("%q is not a list: %v", tc.envelope, err)
			}
			if len(objects) == 0 {
				t.Fatalf("%q is empty; the case cannot check anything", tc.envelope)
			}

			allowed := map[string]bool{}
			for _, k := range append(append([]string{}, tc.required...), tc.optional...) {
				allowed[k] = true
			}
			for i, obj := range objects {
				for _, need := range tc.required {
					if _, ok := obj[need]; !ok {
						t.Errorf("object %d is missing documented field %q\nreference: %s",
							i, need, tc.source)
					}
				}
				// The check that matters: nothing outside the reference.
				for got := range obj {
					if !allowed[got] {
						t.Errorf("object %d carries %q, which the reference does not document.\n"+
							"Either it is invented, or the table here is stale — check %s "+
							"before adding it to the allow-list.", i, got, tc.source)
					}
				}
			}
		})
	}
}

// Connection credentials are write-only, so they are checked at the request
// boundary instead: a credential type must reject fields the reference does
// not define for it. This is the exact shape of the accessKeyId bug.
func TestConnectionCredentialsRejectUndocumentedFields(t *testing.T) {
	f := newFixture(t)

	// The documented CredentialType members (create-connection reference).
	// Anything outside this set must not be accepted as a credential type.
	for _, undocumented := range []string{"AccessKey", "S3AccessKey", "AwsKey"} {
		resp := f.call("POST", "/v1/connections", f.token, map[string]any{"connectionDetails": map[string]any{"type": "WebForPipeline", "creationMethod": "WebForPipeline.Contents", "parameters": []map[string]any{{"dataType": "Text", "name": "baseUrl", "value": "https://x.example"}}},
			"displayName":      "cred-" + undocumented,
			"connectivityType": "ShareableCloud",
			"credentialDetails": map[string]any{"credentials": map[string]any{
				"credentialType": undocumented,
				"username":       "a",
				"password":       "b",
			}},
		}, nil)
		if resp.StatusCode < 400 {
			t.Errorf("credentialType %q was accepted (HTTP %d); the documented enum is "+
				"Windows/Anonymous/Basic/Key/OAuth2/WindowsWithoutImpersonation/"+
				"SharedAccessSignature/ServicePrincipal/WorkspaceIdentity/KeyPair",
				undocumented, resp.StatusCode)
		}
	}

	// And the documented two-secret carrier still works: Fabric's S3 connector
	// collects an Access Key Id and a Secret Access Key, which travel as Basic.
	f.mustStatus(f.call("POST", "/v1/connections", f.token, map[string]any{"connectionDetails": map[string]any{"type": "WebForPipeline", "creationMethod": "WebForPipeline.Contents", "parameters": []map[string]any{{"dataType": "Text", "name": "baseUrl", "value": "https://x.example"}}},
		"displayName":      "cred-basic",
		"connectivityType": "ShareableCloud",
		"credentialDetails": map[string]any{"credentials": map[string]any{
			"credentialType": "Basic", "username": "AKIA...", "password": "secret",
		}},
	}, nil), http.StatusCreated, "documented Basic credential")
}

func keysOfRaw(m map[string]json.RawMessage) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// Connections get their own case because the interesting checks are nested,
// and because this is the surface where a schema bug actually shipped: the
// credential object grew invented `accessKeyId`/`secretAccessKey` fields.
//
// The reference's read model is deliberately narrow — `ListCredentialDetails`
// carries only credentialType, singleSignOnType, connectionEncryption and
// skipTestConnection. No credentials come back at all, which makes "no secret
// ever appears in a list response" a property worth enforcing rather than
// assuming.
func TestConnectionPayloadMatchesDocumentedSchema(t *testing.T) {
	const source = "rest/api/fabric/core/connections/list-connections"
	f := newFixture(t)

	f.mustStatus(f.call("POST", "/v1/connections", f.token, map[string]any{"connectionDetails": map[string]any{"type": "WebForPipeline", "creationMethod": "WebForPipeline.Contents", "parameters": []map[string]any{{"dataType": "Text", "name": "baseUrl", "value": "https://x.example"}}},
		"displayName":      "schema-conn",
		"connectivityType": "ShareableCloud",
		"credentialDetails": map[string]any{
			"credentials": map[string]any{
				"credentialType": "Basic",
				"username":       "sentinel-user",
				"password":       "sentinel-password-do-not-leak",
			}},
	}, nil), http.StatusCreated, "create connection")

	var page struct {
		Value []map[string]json.RawMessage `json:"value"`
	}
	f.mustStatus(f.call("GET", "/v1/connections", f.token, nil, &page),
		http.StatusOK, "list connections")
	if len(page.Value) == 0 {
		t.Fatal("no connections returned")
	}

	// ShareableCloudConnection, per the reference.
	allowed := map[string]bool{}
	for _, k := range []string{
		"id", "displayName", "gatewayId", "connectivityType", "connectionDetails",
		"privacyLevel", "credentialDetails", "connectionRecency",
		"allowConnectionUsageInGateway", "allowUsageInUserControlledCode",
	} {
		allowed[k] = true
	}
	nested := map[string]map[string]bool{
		// ListConnectionDetails
		"connectionDetails": {"path": true, "type": true},
		// ListCredentialDetails — note: no credentials.
		"credentialDetails": {
			"credentialType": true, "singleSignOnType": true,
			"connectionEncryption": true, "skipTestConnection": true,
		},
	}

	for i, conn := range page.Value {
		for _, need := range []string{"id", "displayName", "connectivityType"} {
			if _, ok := conn[need]; !ok {
				t.Errorf("connection %d is missing %q\nreference: %s", i, need, source)
			}
		}
		for got, raw := range conn {
			if !allowed[got] {
				t.Errorf("connection %d carries %q, undocumented by %s", i, got, source)
				continue
			}
			fields, checked := nested[got]
			if !checked {
				continue
			}
			var obj map[string]json.RawMessage
			if json.Unmarshal(raw, &obj) != nil {
				continue
			}
			for inner := range obj {
				if !fields[inner] {
					t.Errorf("connection %d: %s.%s is undocumented by %s",
						i, got, inner, source)
				}
			}
		}
	}

	// No secret may appear anywhere in the response, at any depth. The
	// reference returns no credentials, so a leak is both a schema break and a
	// security bug — and a substring check catches it wherever it hides.
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sentinel-password-do-not-leak", "sentinel-user"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the connections list leaked %q; the reference returns no credentials", secret)
		}
	}
	for _, field := range []string{"password", "secretAccessKey", "servicePrincipalSecret", "privateKey"} {
		if strings.Contains(string(raw), `"`+field+`"`) {
			t.Errorf("the connections list exposes a %q field; ListCredentialDetails has none", field)
		}
	}
}

// Shortcuts, like connections, carry a nested object worth checking: `target`
// holds exactly one of nine documented data-source shapes plus a `type`
// discriminator, and each shape has its own field set. The ADLS and S3 targets
// are the two the emulator resolves for real, so a drift here would break the
// read-through paths silently.
func TestShortcutPayloadMatchesDocumentedSchema(t *testing.T) {
	const source = "rest/api/fabric/core/onelake-shortcuts/list-shortcuts"
	f := newFixture(t)

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "sc-schema"}, &ws)
	var lake struct{ ID string }
	f.call("POST", "/v1/workspaces/"+ws.ID+"/lakehouses", f.token,
		map[string]any{"displayName": "lh"}, &lake)
	var conn struct{ ID string }
	f.call("POST", "/v1/connections", f.token, map[string]any{"connectionDetails": map[string]any{"type": "WebForPipeline", "creationMethod": "WebForPipeline.Contents", "parameters": []map[string]any{{"dataType": "Text", "name": "baseUrl", "value": "https://x.example"}}},
		"displayName": "sc-conn", "connectivityType": "ShareableCloud",
		"credentialDetails": map[string]any{
			"credentials": map[string]any{"credentialType": "Anonymous"}},
	}, &conn)

	base := "/v1/workspaces/" + ws.ID + "/items/" + lake.ID + "/shortcuts"
	f.call("POST", base, f.token, map[string]any{
		"path": "Files", "name": "toAdls", "target": map[string]any{"adlsGen2": map[string]any{
			"location": "https://acct.dfs.core.windows.net", "subpath": "container/sub",
			"connectionId": conn.ID}}}, nil)
	f.call("POST", base, f.token, map[string]any{
		"path": "Files", "name": "toS3", "target": map[string]any{"amazonS3": map[string]any{
			"location": "https://b.s3.us-west-2.amazonaws.com", "subpath": "folder",
			"connectionId": conn.ID}}}, nil)

	var page struct {
		Value []map[string]json.RawMessage `json:"value"`
	}
	f.mustStatus(f.call("GET", base, f.token, nil, &page), http.StatusOK, "list shortcuts")
	if len(page.Value) < 2 {
		t.Fatalf("expected the two shortcuts, got %d", len(page.Value))
	}

	shortcutFields := map[string]bool{
		"name": true, "path": true, "target": true,
		"isShortcutTransform": true, "transform": true,
	}
	// Target: a `type` discriminator plus exactly one data-source object.
	targetFields := map[string]bool{
		"type": true, "adlsGen2": true, "amazonS3": true, "azureBlobStorage": true,
		"dataverse": true, "externalDataShare": true, "googleCloudStorage": true,
		"oneDriveSharePoint": true, "oneLake": true, "s3Compatible": true,
	}
	sourceFields := map[string]map[string]bool{
		"adlsGen2": {"connectionId": true, "location": true, "subpath": true},
		"amazonS3": {"connectionId": true, "location": true, "subpath": true},
		"oneLake":  {"connectionId": true, "itemId": true, "path": true, "workspaceId": true},
	}

	for i, sc := range page.Value {
		for _, need := range []string{"name", "path", "target"} {
			if _, ok := sc[need]; !ok {
				t.Errorf("shortcut %d is missing %q\nreference: %s", i, need, source)
			}
		}
		for got := range sc {
			if !shortcutFields[got] {
				t.Errorf("shortcut %d carries %q, undocumented by %s", i, got, source)
			}
		}
		var target map[string]json.RawMessage
		if json.Unmarshal(sc["target"], &target) != nil {
			t.Errorf("shortcut %d: target is not an object", i)
			continue
		}
		if _, ok := target["type"]; !ok {
			t.Errorf("shortcut %d: target has no `type` discriminator", i)
		}
		bodies := 0
		for got, raw := range target {
			if !targetFields[got] {
				t.Errorf("shortcut %d: target.%s is undocumented by %s", i, got, source)
				continue
			}
			if got == "type" {
				continue
			}
			bodies++
			fields, checked := sourceFields[got]
			if !checked {
				continue
			}
			var obj map[string]json.RawMessage
			if json.Unmarshal(raw, &obj) != nil {
				continue
			}
			for inner := range obj {
				if !fields[inner] {
					t.Errorf("shortcut %d: target.%s.%s is undocumented by %s",
						i, got, inner, source)
				}
			}
		}
		// "must specify exactly one of the supported destinations".
		if bodies > 1 {
			t.Errorf("shortcut %d: target carries %d data-source objects; exactly one is allowed",
				i, bodies)
		}
	}
}
