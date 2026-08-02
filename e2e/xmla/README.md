# XMLA — the ADOMD.NET client contract

A **client-contract oracle**, not an emulator test. The emulator implements no
XMLA surface; [docs/24](../../docs/24-parity-completion.md) defers it on cost.
What this pins down is what a real XMLA client *demands* — the exact first call
any future implementation would have to answer — and the platform facts that
make such an implementation testable at all.

```bash
uv run --frozen --no-sync python e2e/xmla/run.py
```

Needs Docker (the client runs in `mcr.microsoft.com/dotnet/sdk:8.0`) and
`openssl`. Without them it skips — unless `XMLA_REQUIRE=1`, which CI sets, so a
missing prerequisite in CI fails rather than silently reporting success.

## Why it exists

[docs/18](../../docs/18-semantic-model-references.md) recorded XMLA as
deferred-with-cause on two grounds: the client is "native .NET, **not
endpoint-overridable**", and there is no CI oracle. **Both were false**, and the
only thing that established it was pointing the client at a listener and reading
what came out.

That belief cost roadmap for months. A claim that expensive when wrong should be
a check, not a memory — which is why this is a suite rather than a note.

## What it asserts, all measured off the wire

Microsoft's own ADOMD.NET client
(`Microsoft.AnalysisServices.AdomdClient.NetCore.retail.amd64`) on **Linux**,
.NET 8, in a container:

1. **restores, loads and runs** — `PLATFORM Unix/X64`;
2. **is endpoint-overridable.** `Data Source=powerbi://<host>:<port>/v1.0/myorg/<ws>`
   sends TLS to whatever host you name;
3. **trusts a self-signed CA** by the ordinary `update-ca-certificates` route,
   which is what every other e2e here already does for localhost TLS. A rejected
   chain would show up as a completed TCP connect with no HTTP request, and the
   harness reports that case as such rather than as "nothing connected";
4. **takes a bearer token from the connection string** (`Password=<token>`), so
   nothing in the credential path is interactive or Windows-only — asserted by
   reading the exact token back out of the `Authorization` header;
5. **its first call is plain JSON REST, not SOAP:**

   ```
   GET /powerbi/databases/v201606/workspaces?PreferClientRouting=true
   User-Agent: ASClient/.NET-Core
   Authorization: Bearer <token from the connection string>
   ```

   This is the assertion that can change under us when ADOMD.NET ships a new
   version, and the reason the suite runs weekly rather than once.

   Recording the requests rather than printing them surfaced something the
   original one-off measurement missed: **each connection makes the call twice**
   — once with `PreferClientRouting=true`, then immediately again without it.
   That is what the client does when the first call is refused with a 404; this
   harness has never seen it against a server that answers, so whether the
   second call is a fallback or unconditional is not established here. Either
   way, an implementation should expect both forms;
6. the **`Data Source=https://…/xmla` and bare `host:port` forms remain
   Windows-only** on .NET Core —

   ```
   NotSupportedException: This feature is supported for a .NET Core client
   only on Windows systems...
   ```

   so a Linux oracle must use the `powerbi://` form. That is also the form the
   Service documents, so it is the realistic one anyway. Asserted in both
   directions: if a later release lifts the restriction, this suite fails and
   says so.

## What it does NOT establish

The client never reaches XMLA/SOAP — it is still in workspace **routing** when
the capture stub refuses it. So this says nothing about how much of
`[MS-SSAS-T]` a useful implementation needs, and the `L` sizing in `docs/24` is
unchanged. **Feasibility is measured here; cost is not.**

## How the capture stub behaves, and why

`run.py`'s listener answers the routing `GET` with a 404 and any `POST` with a
SOAP fault. That is deliberate, not lazy: answering the routing call would send
the client down a different path, and the recorded first-call contract would no
longer be the one a fresh client performs.

## Ruled out

`xmla` (the pure-Python `olap.xmla` client, PyPI 0.7.2) is unusable: it pins
`requests == 1.2.3`, a 2013 release, and fails to build on Python ≥ 3.10.
