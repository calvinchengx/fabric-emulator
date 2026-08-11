package api

// The Create Connection request contract, as a real tenant enforces it.
//
// MEASURED 2026-08-11 against a Fabric trial, by POSTing the shape these
// examples had always used and by reading the tenant's own
// `GET /v1/connections/supportedConnectionTypes?showAllCreationMethods=true`
// (321 types). Two things were wrong and neither could be seen locally, because
// the emulator accepted both:
//
//  1. `connectionDetails` was sent as `{type, path}`. `path` is the RESPONSE
//     shape (ListConnectionDetails); the request shape is
//     `{type, creationMethod, parameters[]}`. The tenant answered:
//
//     {"errorCode":"InvalidParameter","message":"The CreationMethod field is required."}
//
//  2. `credentialType: "AzureKeyVaultReference"` does not exist. The documented
//     enum is the ten values in credentialTypes below. Fabric DOES reference
//     vault secrets — but as a KeyVaultSecretReference nested inside a
//     credential (`keyReference`, `passwordReference`, `tokenReference`,
//     `servicePrincipalSecretReference`), shaped
//     `{connectionId, secretName, version}` where connectionId names a
//     CONNECTION TO THE VAULT, not a vaultUri.
//
// WHY REJECT RATHER THAN KEEP ACCEPTING BOTH. The invented shape worked here
// and only here, so every example that used it was unportable in a way no local
// run could reveal — the emulator being more permissive than the tenant, which
// is the failure direction that ships. Accepting both shapes on one surface
// would leave that signal destroyed for the sake of payloads that no tenant has
// ever accepted.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// credentialTypes is Fabric's CredentialType enum, verbatim.
// https://learn.microsoft.com/en-us/rest/api/fabric/core/connections/create-connection
var credentialTypes = []string{
	"Anonymous", "Basic", "Key", "KeyPair", "OAuth2", "ServicePrincipal",
	"SharedAccessSignature", "Windows", "WindowsWithoutImpersonation",
	"WorkspaceIdentity",
}

// connectionCredentialTypes is which credential types a given connection type
// accepts, from the tenant's own
// `GET /v1/connections/supportedConnectionTypes?showAllCreationMethods=true`
// (321 types; only the ones this repo exercises are listed here).
//
// A type absent from this map is UNCONSTRAINED, deliberately: shipping a
// partial table as if it were complete would refuse working payloads for
// connectors nobody has measured. The entries present are measured, and the
// silence elsewhere is honest rather than permissive-by-accident.
//
// WHY THIS EXISTS AT ALL. The first version of the vault-reference support
// required the AzureKeyVault connection to carry `WorkspaceIdentity` — which is
// the one credential this table shows Fabric does NOT accept for it:
//
//	400 UnsupportedCredentialType
//	The CredentialType input is not supported for this API
//
// The measurement was in hand when that code was written and only part of the
// line was read. So the emulator enforced the opposite of the contract, and a
// green local run said nothing.
var connectionCredentialTypes = map[string][]string{
	"AzureKeyVault":        {"OAuth2", "ServicePrincipal"},
	"Web":                  {"Anonymous", "Basic", "OAuth2", "ServicePrincipal"},
	"WebForPipeline":       {"OAuth2", "Basic", "Anonymous", "Key", "ServicePrincipal"},
	"AzureDataLakeStorage": {"Key", "OAuth2", "SharedAccessSignature", "ServicePrincipal", "WorkspaceIdentity"},
	"AmazonS3":             {"Basic", "OAuth2", "ServicePrincipal"},
	"AzureBlobs":           {"Anonymous", "Key", "OAuth2", "SharedAccessSignature", "ServicePrincipal", "WorkspaceIdentity"},
}

// validateCredentialForConnection refuses a credential the connector does not
// take, with the tenant's own error text.
func validateCredentialForConnection(connType, credType string) string {
	allowed, known := connectionCredentialTypes[connType]
	if !known || credType == "" {
		return ""
	}
	if slices.ContainsFunc(allowed, func(a string) bool { return strings.EqualFold(a, credType) }) {
		return ""
	}
	return fmt.Sprintf("The CredentialType input is not supported for this API. "+
		"Connection type %q accepts: %s.", connType, strings.Join(allowed, ", "))
}

// vaultRefFields maps each credential field that may carry a
// KeyVaultSecretReference to the credentialType that owns it. A reference on
// any other field is a different credential's, and naming which one is the
// difference between a usable error and "invalid input".
var vaultRefFields = map[string]string{
	"keyReference":                    "Key",
	"passwordReference":               "Basic",
	"tokenReference":                  "SharedAccessSignature",
	"servicePrincipalSecretReference": "ServicePrincipal",
}

// keyVaultSecretReference is Fabric's pointer to a secret. `connectionId` names
// a connection whose own type is AzureKeyVault — the vault is reached through
// ITS credentials, which is why a vaultUri never appears here.
type keyVaultSecretReference struct {
	ConnectionID string `json:"connectionId"`
	SecretName   string `json:"secretName"`
	Version      string `json:"version,omitempty"`
}

// createConnectionDetails is the REQUEST shape. Note the absence of `path`.
type createConnectionDetails struct {
	Type           string            `json:"type"`
	CreationMethod string            `json:"creationMethod"`
	Parameters     []json.RawMessage `json:"parameters"`
	// Path is declared solely to DETECT it: a caller sending it has copied the
	// response shape into a request, which is the mistake these examples made.
	Path *string `json:"path"`
}

// validateConnectionDetails reports the tenant's own complaint, or "".
func validateConnectionDetails(raw json.RawMessage) (createConnectionDetails, string) {
	var d createConnectionDetails
	if len(raw) == 0 {
		return d, "connectionDetails is required."
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, "connectionDetails is not an object: " + err.Error()
	}
	if d.Type == "" {
		return d, "The Type field is required."
	}
	if d.CreationMethod == "" {
		// The tenant's wording, kept verbatim so a search for the error a real
		// deploy produced lands here.
		return d, "The CreationMethod field is required. " +
			"Creation methods and their parameters are listed by " +
			"GET /v1/connections/supportedConnectionTypes."
	}
	if d.Path != nil {
		return d, "connectionDetails.path is part of the connection READ shape, " +
			"not the create request: send creationMethod and parameters instead."
	}
	// A creation method names its parameters; every measured type had at least
	// one required parameter, and a create with none silently connects to
	// nothing.
	if len(d.Parameters) == 0 {
		return d, fmt.Sprintf("Creation method %q requires parameters "+
			"(each {dataType, name, value}).", d.CreationMethod)
	}
	return d, ""
}

// testVaultReference resolves the secret a reference points at, the way Fabric
// tests a connection at create time. Returns "" on success.
//
// The route is: reference -> AzureKeyVault connection -> its `accountName`
// parameter -> the vault. Every hop can be missing, and each says which,
// because "invalid input" against a four-hop chain is the least useful thing
// this could report.
func (a *API) testVaultReference(ref *keyVaultSecretReference) string {
	vaultConn, err := a.Store.GetConnection(ref.ConnectionID)
	if err != nil {
		return fmt.Sprintf("no connection %s — connectionId must name a "+
			"connection of type AzureKeyVault.", ref.ConnectionID)
	}
	var vd createConnectionDetails
	if len(vaultConn.Details) > 0 {
		_ = json.Unmarshal(vaultConn.Details, &vd)
	}
	if !strings.EqualFold(vd.Type, "AzureKeyVault") {
		return fmt.Sprintf("connection %s is of type %q, not AzureKeyVault.",
			ref.ConnectionID, vd.Type)
	}
	account := connectionParameter(vd.Parameters, "accountName")
	if account == "" {
		return fmt.Sprintf("connection %s has no accountName parameter.",
			ref.ConnectionID)
	}
	if a.Entra == nil || a.AKV == nil {
		return "no Entra/vault endpoint is configured to resolve the reference " +
			"(set skipTestConnection to bypass)."
	}
	// The vault connection's OWN credentials authenticate to the vault — that
	// is the whole point of routing through a connection, and which credential
	// is admissible is the connector's business, not ours: AzureKeyVault takes
	// OAuth2 or ServicePrincipal and nothing else.
	var vaultCred connectionCredentials
	if vaultConn.CredentialsJSON != "" {
		_ = json.Unmarshal([]byte(vaultConn.CredentialsJSON), &vaultCred)
	}
	bearer, msg := a.vaultBearer(vaultCred)
	if msg != "" {
		return msg
	}
	if _, err := a.AKV.ResolveSecret(a.AKV.VaultURI(account), ref.SecretName, bearer); err != nil {
		return err.Error()
	}
	return ""
}

// vaultBearer mints the vault-audience token the vault connection's own
// credential entitles it to, or explains why it cannot.
//
// ServicePrincipal is the path a real tenant supports and the only one that is
// automatable: OAuth2 needs interactive consent, which no example can perform.
// WorkspaceIdentity is accepted ONLY as a local convenience for stacks with no
// service principal to hand, and it is named as such — a tenant refuses it for
// this connector, so anything relying on it here is emulator-only.
func (a *API) vaultBearer(cred connectionCredentials) (string, string) {
	if a.Entra == nil || a.AKV == nil {
		return "", "no Entra/vault endpoint is configured to resolve the reference " +
			"(set skipTestConnection to bypass)."
	}
	switch cred.CredentialType {
	case "ServicePrincipal":
		bearer, err := a.Entra.MintServicePrincipalToken(
			cred.TenantID, cred.ServicePrincipalClientID, cred.ServicePrincipalSecret,
			"https://vault.azure.net/.default")
		if err != nil {
			return "", err.Error()
		}
		return bearer, ""
	case "WorkspaceIdentity":
		wi, err := a.Store.GetWorkspaceIdentity(cred.WorkspaceID)
		if err != nil {
			return "", "the vault connection's workspace has no provisioned identity."
		}
		bearer, err := a.Entra.MintWorkspaceIdentityToken(wi.IdentityID, "https://vault.azure.net")
		if err != nil {
			return "", err.Error()
		}
		return bearer, ""
	default:
		return "", fmt.Sprintf("the vault connection carries %q credentials; "+
			"resolving a secret needs ServicePrincipal (OAuth2 requires "+
			"interactive consent and cannot be completed by a script).",
			cred.CredentialType)
	}
}

// connectionParameter reads one `{dataType, name, value}` entry by name.
func connectionParameter(params []json.RawMessage, name string) string {
	for _, raw := range params {
		var p struct {
			Name  string `json:"name"`
			Value any    `json:"value"`
		}
		if json.Unmarshal(raw, &p) == nil && strings.EqualFold(p.Name, name) && p.Value != nil {
			return fmt.Sprint(p.Value)
		}
	}
	return ""
}

// validateCredentialType rejects anything outside Fabric's enum, and singles
// out the shape these examples used because a bare "unknown credentialType"
// would not tell anyone what to write instead.
func validateCredentialType(t string) string {
	if t == "" {
		return "credentialDetails.credentials.credentialType is required."
	}
	if strings.EqualFold(t, "AzureKeyVaultReference") {
		return "AzureKeyVaultReference is not a Fabric credentialType. " +
			"To resolve a credential from Azure Key Vault, use the owning type " +
			"with a secret reference — e.g. credentialType \"Key\" with " +
			"keyReference {connectionId, secretName} — where connectionId names " +
			"a connection of type AzureKeyVault."
	}
	if !slices.Contains(credentialTypes, t) {
		return fmt.Sprintf("Unknown credentialType %q. Supported: %s.",
			t, strings.Join(credentialTypes, ", "))
	}
	return ""
}
