"""A3 — bring the third source in: Contoso ERP, plus the reference feeds.

Third system, third credential: its own Key Vault secret and its own
AKV-reference connection, for the same reason Contoso Web got one in 20.

What is new here is the SHAPE of what lands. POS sends CSV and JSON Lines; Web
sends nested JSON. The ERP connector lands **Parquet** — which is what a CDC
connector actually writes — and so does the reference publisher. Landing stays
verbatim either way: the file is stored as sent, columnar or not.
"""
import datetime

import erp_system as erp
import reference_data as ref
from common import (FABRIC, KV, KV_INTERNAL, S, STORAGE_AUD, VAULT_AUD, ensure_app,
                    fabric_headers, load, log, save, token)

ensure_app(VAULT_AUD, "Azure Key Vault")
vt = token(VAULT_AUD)
st = load()

ensure_app(STORAGE_AUD, "Azure Storage")
st_tok = token(STORAGE_AUD)
today = datetime.date.today().isoformat()
conns = {}

# Two publishers, two secrets, two connections — reference data is not
# automatically public just because it is not transactional.
for label, mod, path in [
    ("contoso-erp", erp, "https://erp.contoso.example/api/cdc/customers"),
    ("contoso-reference", ref, "https://ref.contoso.example/api/v1/publish"),
]:
    r = S.put(f"{KV}/secrets/{label}-api-key?api-version=7.4",
              headers={"Authorization": "Bearer " + vt}, json={"value": mod.API_KEY})
    assert r.status_code in (200, 201), f"put secret {label}: {r.status_code} {r.text}"

    r = S.post(f"{FABRIC}/v1/connections", headers=fabric_headers(), json={
        "displayName": label,
        "connectivityType": "ShareableCloud",
        "connectionDetails": {"type": "RestApi", "path": path},
        "credentialDetails": {"credentials": {
            "credentialType": "AzureKeyVaultReference",
            "workspaceId": st["workspace"],
            "vaultUri": KV_INTERNAL,
            "secretName": f"{label}-api-key"}}})
    assert r.status_code == 201, f"AKV connection {label}: {r.status_code} {r.text}"
    conns[label] = r.json()["id"]
    log(f"AKV-reference connection {conns[label]} for {label}")

    # The vendor's gate is real, not decorative — same check the other two get.
    try:
        mod.export("wrong-key")
        raise AssertionError(f"{label} accepted a wrong API key")
    except PermissionError:
        pass

# Four connections now exist across three source systems and one publisher.
# None of them may echo a secret back.
listed = S.get(f"{FABRIC}/v1/connections", headers=fabric_headers())
listed.raise_for_status()
for mod in (erp, ref):
    assert mod.API_KEY not in listed.text, "connection listing leaked a secret"

for folder, mod, secret in [("contoso_erp", erp, "contoso-erp-api-key"),
                            ("reference", ref, "contoso-reference-api-key")]:
    r = S.get(f"{KV}/secrets/{secret}?api-version=7.4",
              headers={"Authorization": "Bearer " + vt})
    r.raise_for_status()
    for name, blob in mod.export(r.json()["value"]).items():
        path = f"Files/landing/{folder}/{today}/{name}"
        r2 = S.put(f"{FABRIC}/onelake/{st['workspace']}/{st['lakehouse']}/{path}",
                   data=blob, headers={"Authorization": "Bearer " + st_tok,
                                       "x-ms-blob-type": "BlockBlob"})
        assert r2.status_code in (200, 201), f"land {path}: {r2.status_code} {r2.text}"
        log(f"landed {path} ({len(blob)} bytes)")

save(erp_connection=conns["contoso-erp"],
     reference_connection=conns["contoso-reference"],
     erp_landing_date=today)
