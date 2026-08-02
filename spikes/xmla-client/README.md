# Spike: is a real XMLA client CI-runnable against a server we control?

`docs/18` recorded XMLA as deferred-with-cause on two grounds — the client is
"native .NET, **not endpoint-overridable**", and there is no CI oracle. This
spike tested both. **Both are false.**

## Run it

```bash
python3 tlscap.py &                      # TLS listener + request log, port 18080
openssl req -x509 -newkey rsa:2048 -keyout key.pem -out cert.pem -days 2 -nodes \
  -subj "/CN=host.docker.internal" \
  -addext "subjectAltName=DNS:host.docker.internal,DNS:localhost,IP:127.0.0.1"
docker run --rm --platform linux/amd64 -v "$PWD:/src" -w /src \
  mcr.microsoft.com/dotnet/sdk:8.0 \
  sh -c "cp /src/cert.pem /usr/local/share/ca-certificates/ca.crt && \
         update-ca-certificates >/dev/null 2>&1; dotnet run"
```

## What it establishes

Microsoft's own ADOMD.NET client (`Microsoft.AnalysisServices.AdomdClient.NetCore.retail.amd64`)
on **Linux**, .NET 8, in a container:

1. **restores and loads** — `adomd on Unix / X64`;
2. **is endpoint-overridable.** `Data Source=powerbi://<host>:<port>/v1.0/myorg/<ws>`
   sends TLS to whatever host you name. Confirmed at the TCP layer first
   (ClientHello, SNI = our host) and then at the HTTP layer;
3. **trusts a self-signed CA** by the ordinary `update-ca-certificates` route,
   which is what every other e2e here already does for localhost TLS;
4. **takes a bearer token from the connection string** (`Password=<token>`), so
   no interactive auth and nothing Windows-only in the credential path;
5. **its first call is plain JSON REST, not SOAP:**

   ```
   GET /powerbi/databases/v201606/workspaces?PreferClientRouting=true
   User-Agent: ASClient/.NET-Core
   Authorization: Bearer <token from the connection string>
   Content-Type: application/json
   ```

## What it does NOT establish

The client never reached XMLA/SOAP — it is still in workspace **routing** when
our stub 500s it. So this says nothing about how much of `[MS-SSAS-T]` a useful
implementation needs, and the `L` sizing in `docs/24` is unchanged. Feasibility
moved; cost did not.

One real constraint found: the **`Data Source=http://…/xmla` and `host:port`
forms are Windows-only** —

    NotSupportedException: This feature is supported for a .NET Core client
    only on Windows systems...

so a Linux oracle must use the `powerbi://` form. That is also the form the
Service documents, so it is the realistic one anyway.

## Ruled out

`xmla` (the pure-Python `olap.xmla` client, PyPI 0.7.2) is unusable: it pins
`requests == 1.2.3`, a 2013 release, and fails to build on Python ≥ 3.10.
