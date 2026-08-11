package api

// The create-connection contract's own branches, each asserted by the answer it
// gives rather than by the fact that it refused.
//
// A wrong refusal and a right one are both "400" at the call site, and this
// surface exists precisely because a tenant's own "The request has an invalid
// input" sent us hunting the wrong field for an afternoon. So every case here
// checks the message names the thing the caller must change.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/akv"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func TestConnectionDetailsRefusalsNameTheField(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"absent", ``, "connectionDetails is required"},
		{"not an object", `"a string"`, "not an object"},
		{"no type", `{"creationMethod":"m","parameters":[{}]}`, "Type field is required"},
		{"no creationMethod", `{"type":"Web","parameters":[{}]}`, "CreationMethod field is required"},
		{"read shape sent as a request", `{"type":"Web","creationMethod":"Web","path":"x","parameters":[{}]}`,
			"READ shape"},
		{"no parameters", `{"type":"Web","creationMethod":"Web"}`, "requires parameters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, msg := validateConnectionDetails(json.RawMessage(tc.body))
			if !strings.Contains(msg, tc.want) {
				t.Errorf("msg = %q, want it to mention %q", msg, tc.want)
			}
		})
	}

	// The shape a tenant accepts passes, so the cases above are refusals rather
	// than a function that refuses everything.
	if _, msg := validateConnectionDetails(json.RawMessage(
		`{"type":"Web","creationMethod":"Web","parameters":[{"dataType":"Text","name":"url","value":"https://x"}]}`,
	)); msg != "" {
		t.Errorf("a valid details object was refused: %s", msg)
	}
}

func TestCredentialIsCheckedAgainstTheConnector(t *testing.T) {
	// Measured pairs: AzureKeyVault takes ServicePrincipal and refuses
	// WorkspaceIdentity — the inversion that shipped in #190.
	if msg := validateCredentialForConnection("AzureKeyVault", "ServicePrincipal"); msg != "" {
		t.Errorf("ServicePrincipal on AzureKeyVault refused: %s", msg)
	}
	msg := validateCredentialForConnection("AzureKeyVault", "WorkspaceIdentity")
	if !strings.Contains(msg, "not supported") || !strings.Contains(msg, "ServicePrincipal") {
		t.Errorf("msg = %q; want it to refuse and list what IS accepted", msg)
	}

	// An UNMEASURED connector is unconstrained on purpose. Shipping a partial
	// table as if it were complete would refuse working payloads for connectors
	// nobody has captured, which is a worse failure than permitting one.
	if msg := validateCredentialForConnection("SomeConnectorNobodyMeasured", "Anonymous"); msg != "" {
		t.Errorf("unmeasured connector constrained: %s", msg)
	}
	// No credential to check is not a violation: credentialDetails is optional.
	if msg := validateCredentialForConnection("AzureKeyVault", ""); msg != "" {
		t.Errorf("empty credentialType refused: %s", msg)
	}
}

func TestVaultBearerRefusesCredentialsItCannotUse(t *testing.T) {
	a, _ := newAPI(t)

	// No Entra/AKV configured: the contract-only stack says so rather than
	// failing later inside a mint.
	if _, msg := a.vaultBearer(connectionCredentials{CredentialType: "ServicePrincipal"}); !strings.Contains(msg, "no Entra/vault endpoint") {
		t.Errorf("unconfigured stack msg = %q", msg)
	}

	a.Entra = wiEntra(t, false)
	a.AKV = akv.New(false, nil, "vault.example:443")

	// OAuth2 is a REAL Fabric credential for this connector and still cannot be
	// completed by a script — the message has to say which of those it is, or a
	// reader goes looking for a bug that is not there.
	_, msg := a.vaultBearer(connectionCredentials{CredentialType: "OAuth2"})
	if !strings.Contains(msg, "interactive consent") || !strings.Contains(msg, "ServicePrincipal") {
		t.Errorf("OAuth2 msg = %q; want it to name consent and the alternative", msg)
	}

	// WorkspaceIdentity works locally, and only with a provisioned identity.
	_, msg = a.vaultBearer(connectionCredentials{
		CredentialType: "WorkspaceIdentity", WorkspaceID: "no-such-workspace"})
	if !strings.Contains(msg, "no provisioned identity") {
		t.Errorf("unprovisioned msg = %q", msg)
	}
}

// The path a tenant actually supports: the vault connection's own service
// principal mints the vault-audience token.
func TestVaultBearerMintsForAServicePrincipal(t *testing.T) {
	a, _ := newAPI(t)
	a.AKV = akv.New(false, nil, "vault.example:443")

	a.Entra = wiEntra(t, false)
	sp := connectionCredentials{
		CredentialType: "ServicePrincipal", TenantID: "t",
		ServicePrincipalClientID: "c", ServicePrincipalSecret: "s",
	}
	bearer, msg := a.vaultBearer(sp)
	if msg != "" {
		t.Fatalf("ServicePrincipal mint refused: %s", msg)
	}
	if bearer == "" {
		t.Error("no bearer returned")
	}

	// A rejected credential surfaces entra's own complaint rather than a
	// generic failure — the difference between fixing a secret and hunting a
	// connector.
	a.Entra = wiEntra(t, true)
	if _, msg := a.vaultBearer(sp); !strings.Contains(msg, "rejected") {
		t.Errorf("failed mint msg = %q, want entra's rejection", msg)
	}
}

// The refusals that fire before a connector is even consulted.
func TestCredentialTypeIsCheckedAgainstFabricsEnum(t *testing.T) {
	if msg := validateCredentialType(""); !strings.Contains(msg, "is required") {
		t.Errorf("empty credentialType msg = %q", msg)
	}
	// The invented type gets its own answer naming the replacement, because
	// "unknown credentialType" would leave the reader exactly where the
	// tenant's own "invalid input" left us.
	msg := validateCredentialType("AzureKeyVaultReference")
	if !strings.Contains(msg, "keyReference") || !strings.Contains(msg, "AzureKeyVault") {
		t.Errorf("AzureKeyVaultReference msg = %q", msg)
	}
	if msg := validateCredentialType("Kerberos"); !strings.Contains(msg, "Unknown credentialType") {
		t.Errorf("unknown type msg = %q", msg)
	}
	for _, ok := range credentialTypes {
		if msg := validateCredentialType(ok); msg != "" {
			t.Errorf("documented type %q refused: %s", ok, msg)
		}
	}
}

// A vault reference names a connection, so every hop can be missing and each
// says which — "invalid input" against a four-hop chain is useless.
func TestVaultReferenceNamesTheHopThatFailed(t *testing.T) {
	a, st := newAPI(t)

	if msg := a.testVaultReference(&keyVaultSecretReference{ConnectionID: "nope"}); !strings.Contains(msg, "AzureKeyVault") {
		t.Errorf("missing connection msg = %q", msg)
	}

	// Exists, but is not a vault.
	web := &store.Connection{DisplayName: "web",
		Details: []byte(`{"type":"Web","creationMethod":"Web"}`)}
	if err := st.CreateConnection(web); err != nil {
		t.Fatal(err)
	}
	if msg := a.testVaultReference(&keyVaultSecretReference{ConnectionID: web.ID}); !strings.Contains(msg, "not AzureKeyVault") {
		t.Errorf("wrong-type msg = %q", msg)
	}

	// A vault connection with no accountName addresses no vault.
	kv := &store.Connection{DisplayName: "kv",
		Details: []byte(`{"type":"AzureKeyVault","creationMethod":"AzureKeyVault.Actions"}`)}
	if err := st.CreateConnection(kv); err != nil {
		t.Fatal(err)
	}
	if msg := a.testVaultReference(&keyVaultSecretReference{ConnectionID: kv.ID}); !strings.Contains(msg, "accountName") {
		t.Errorf("no-accountName msg = %q", msg)
	}
}
