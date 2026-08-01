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
		resp := f.call("POST", "/v1/connections", f.token, map[string]any{
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
	f.mustStatus(f.call("POST", "/v1/connections", f.token, map[string]any{
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
