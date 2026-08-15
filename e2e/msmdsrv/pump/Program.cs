using System.Globalization;
using System.Text;
using System.Text.Json;
using Microsoft.AnalysisServices;
using Microsoft.AnalysisServices.AdomdClient;
using Tom = Microsoft.AnalysisServices.Tabular;
using STJ = System.Text.Json.JsonSerializer;

// HTTP front for a local msmdsrv. fabric-emulator POSTs DAX here when
// FABRIC_DAX_URL is set (docs/52-msmdsrv-hosts.md). This process must run
// INSIDE the Windows guest — ADOMD's bare host:port form is Windows-only
// on .NET Core, and that is the form a loopback msmdsrv answers.
//
//   POST /v1/dax      {"query":"EVALUATE …","catalog":"…"}  →  {"rows":[…]}
//   POST /v1/deploy   {"tmsl":{createOrReplace…}}           →  {"ok":true}
//   GET  /health                                           →  {"ok":true,"port":"63321"}
//
// Port comes from MSMDSRV_PORT, or from Desktop's msmdsrv.port.txt (UTF-16,
// same file e2e/pbix-desktop already polls). Re-read on every request so a
// Desktop restart that moved the port does not require bouncing the pump.

var listen = Environment.GetEnvironmentVariable("MSMDSRV_PUMP_ADDR") ?? "http://0.0.0.0:8080";
var builder = WebApplication.CreateBuilder(args);
builder.WebHost.UseUrls(listen);
builder.Logging.ClearProviders();
builder.Logging.AddConsole();
var app = builder.Build();

app.MapGet("/health", () =>
{
    try
    {
        var src = Pump.ResolveSource(null);
        using var c = new AdomdConnection(src.ConnectionString);
        c.Open();
        c.Close();
        return Results.Json(new { ok = true, port = src.Port ?? "", catalog = src.Catalog ?? "" });
    }
    catch (Exception e)
    {
        return Results.Json(new { ok = false, error = Pump.FirstLine(e) }, statusCode: 503);
    }
});

app.MapPost("/v1/dax", async (HttpRequest req) =>
{
    QueryBody body;
    try
    {
        body = await STJ.DeserializeAsync<QueryBody>(req.Body, Pump.JsonOpts)
               ?? new QueryBody();
    }
    catch (JsonException e)
    {
        return Results.Json(new { error = new { code = "DAXQueryError", message = e.Message } },
            statusCode: 400);
    }
    if (string.IsNullOrWhiteSpace(body.Query))
    {
        return Results.Json(new { error = new { code = "DAXQueryError", message = "query is required" } },
            statusCode: 400);
    }

    Source src;
    try
    {
        src = Pump.ResolveSource(body.Catalog);
    }
    catch (Exception e)
    {
        return Results.Json(new { error = new { code = "DAXEngineUnreachable", message = Pump.FirstLine(e) } },
            statusCode: 503);
    }

    AdomdConnection conn = null;
    try
    {
        conn = new AdomdConnection(src.ConnectionString);
        conn.Open();
    }
    catch (Exception e)
    {
        conn?.Dispose();
        return Results.Json(new { error = new { code = "DAXEngineUnreachable", message = Pump.FirstLine(e) } },
            statusCode: 503);
    }

    try
    {
        using (conn)
        {
            using var cmd = conn.CreateCommand();
            cmd.CommandText = body.Query;
            using var r = cmd.ExecuteReader();
            return Results.Json(new { rows = Pump.ReadRows(r) }, Pump.JsonOpts);
        }
    }
    catch (Exception e)
    {
        return Results.Json(new { error = new { code = "DAXQueryError", message = Pump.FirstLine(e) } },
            statusCode: 400);
    }
});

app.MapPost("/v1/deploy", async (HttpRequest req) =>
{
    DeployBody body;
    try
    {
        body = await STJ.DeserializeAsync<DeployBody>(req.Body, Pump.JsonOpts)
               ?? new DeployBody();
    }
    catch (JsonException e)
    {
        return Results.Json(new { error = new { code = "DAXDeployError", message = e.Message } },
            statusCode: 400);
    }
    string tmsl = Pump.TmslText(body.Tmsl);
    if (string.IsNullOrWhiteSpace(tmsl))
    {
        return Results.Json(new { error = new { code = "DAXDeployError", message = "tmsl is required" } },
            statusCode: 400);
    }

    Source src;
    try
    {
        // Connect to the instance, not a catalog — CreateOrReplace names the database.
        src = Pump.ResolveSource(null);
    }
    catch (Exception e)
    {
        return Results.Json(new { error = new { code = "DAXEngineUnreachable", message = Pump.FirstLine(e) } },
            statusCode: 503);
    }

    try
    {
        var database = Pump.ExecuteTmsl(src.InstanceConnectionString, tmsl);
        return Results.Json(new { ok = true, database });
    }
    catch (Exception e)
    {
        var msg = Pump.FirstLine(e);
        if (Pump.IsRejected(msg))
        {
            return Results.Json(new { rejected = true, error = new { code = "DAXDeployRejected", message = msg } },
                statusCode: 409);
        }
        return Results.Json(new { error = new { code = "DAXDeployError", message = msg } },
            statusCode: 400);
    }
});

app.Logger.LogInformation("dax pump listening on {addr}", listen);
app.Run();

record QueryBody
{
    public string Query { get; set; }
    public string Catalog { get; set; }
}

record DeployBody
{
    public JsonElement Tmsl { get; set; }
}

record Source(string ConnectionString, string InstanceConnectionString, string Port, string Catalog);

static class Pump
{
    internal static readonly JsonSerializerOptions JsonOpts = new()
    {
        PropertyNameCaseInsensitive = true,
        PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
    };

    internal static List<Dictionary<string, object>> ReadRows(AdomdDataReader r)
    {
        var rows = new List<Dictionary<string, object>>();
        while (r.Read())
        {
            var row = new Dictionary<string, object>(StringComparer.Ordinal);
            for (int i = 0; i < r.FieldCount; i++)
            {
                var name = r.GetName(i);
                if (r.IsDBNull(i))
                {
                    row[name] = null;
                    continue;
                }
                row[name] = Box(r.GetValue(i));
            }
            rows.Add(row);
        }
        return rows;
    }

    static object Box(object val) => val switch
    {
        double d => d,
        float f => (double)f,
        decimal m => (double)m,
        byte or sbyte or short or ushort or int or uint or long or ulong
            => Convert.ToInt64(val, CultureInfo.InvariantCulture),
        bool b => b,
        DateTime dt => dt.ToString("o", CultureInfo.InvariantCulture),
        _ => Convert.ToString(val, CultureInfo.InvariantCulture),
    };

    internal static Source ResolveSource(string catalogOverride)
    {
        var explicitCs = Environment.GetEnvironmentVariable("MSMDSRV_DATA_SOURCE");
        if (!string.IsNullOrWhiteSpace(explicitCs))
        {
            var catalog = FirstNonEmpty(catalogOverride, Environment.GetEnvironmentVariable("MSMDSRV_CATALOG"));
            return new Source(WithCatalog(explicitCs, catalog), StripCatalog(explicitCs), Port: null, Catalog: catalog);
        }
        var port = Environment.GetEnvironmentVariable("MSMDSRV_PORT");
        if (string.IsNullOrWhiteSpace(port))
        {
            port = ReadDesktopPort().ToString(CultureInfo.InvariantCulture);
        }
        var chosen = FirstNonEmpty(catalogOverride, Environment.GetEnvironmentVariable("MSMDSRV_CATALOG"));
        var instance = $"Data Source=localhost:{port};";
        var cs = new StringBuilder(instance);
        if (!string.IsNullOrWhiteSpace(chosen))
        {
            cs.Append("Initial Catalog=").Append(chosen).Append(';');
        }
        return new Source(cs.ToString(), instance, port, chosen);
    }

    static string FirstNonEmpty(params string[] values)
    {
        foreach (var v in values)
        {
            if (!string.IsNullOrWhiteSpace(v))
            {
                return v;
            }
        }
        return null;
    }

    static string WithCatalog(string cs, string catalog)
    {
        if (string.IsNullOrWhiteSpace(catalog))
        {
            return cs;
        }
        if (cs.IndexOf("Initial Catalog=", StringComparison.OrdinalIgnoreCase) >= 0)
        {
            return cs;
        }
        return cs.TrimEnd(';') + ";Initial Catalog=" + catalog + ";";
    }

    static string StripCatalog(string cs)
    {
        var parts = cs.Split(';', StringSplitOptions.RemoveEmptyEntries);
        return string.Join(";", parts.Where(p =>
            !p.StartsWith("Initial Catalog=", StringComparison.OrdinalIgnoreCase))) + ";";
    }

    internal static string TmslText(JsonElement el)
    {
        if (el.ValueKind is JsonValueKind.Undefined or JsonValueKind.Null)
        {
            return null;
        }
        if (el.ValueKind == JsonValueKind.String)
        {
            return el.GetString();
        }
        return el.GetRawText();
    }

    internal static string ExecuteTmsl(string connectionString, string tmsl)
    {
        var server = new Tom.Server();
        server.Connect(connectionString);
        try
        {
            var results = server.Execute(tmsl);
            var errors = new List<string>();
            foreach (XmlaResult r in results)
            {
                foreach (XmlaMessage m in r.Messages)
                {
                    if (m is XmlaError err)
                    {
                        errors.Add(err.Description);
                    }
                }
            }
            if (errors.Count > 0)
            {
                throw new InvalidOperationException(string.Join("; ", errors));
            }
            var name = DatabaseName(tmsl);
            return name ?? "";
        }
        finally
        {
            if (server.Connected)
            {
                server.Disconnect();
            }
        }
    }

    static string DatabaseName(string tmsl)
    {
        try
        {
            using var doc = JsonDocument.Parse(tmsl);
            if (doc.RootElement.TryGetProperty("createOrReplace", out var cor) &&
                cor.TryGetProperty("object", out var obj) &&
                obj.TryGetProperty("database", out var db))
            {
                return db.GetString();
            }
        }
        catch (JsonException)
        {
        }
        return null;
    }

    internal static bool IsRejected(string msg)
    {
        if (string.IsNullOrEmpty(msg))
        {
            return false;
        }
        return msg.Contains("not support", StringComparison.OrdinalIgnoreCase) ||
               msg.Contains("not supported", StringComparison.OrdinalIgnoreCase) ||
               msg.Contains("read-only", StringComparison.OrdinalIgnoreCase) ||
               msg.Contains("readonly", StringComparison.OrdinalIgnoreCase) ||
               msg.Contains("cannot create", StringComparison.OrdinalIgnoreCase) ||
               msg.Contains("operation is not allowed", StringComparison.OrdinalIgnoreCase);
    }

    static int ReadDesktopPort()
    {
        var local = Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData);
        if (string.IsNullOrEmpty(local) || !Directory.Exists(local))
        {
            throw new InvalidOperationException("MSMDSRV_PORT is unset and %LOCALAPPDATA% is missing");
        }
        var newest = Directory.EnumerateFiles(local, "msmdsrv.port.txt", SearchOption.AllDirectories)
            .Where(p => p.Contains("Power BI Desktop", StringComparison.OrdinalIgnoreCase))
            .Select(p => new FileInfo(p))
            .Where(f => f.Length > 0)
            .OrderByDescending(f => f.LastWriteTimeUtc)
            .FirstOrDefault();
        if (newest == null)
        {
            throw new InvalidOperationException(
                "MSMDSRV_PORT is unset and no Desktop msmdsrv.port.txt was found — open a .pbix or set the port");
        }
        return ParsePortFile(File.ReadAllBytes(newest.FullName));
    }

    // UTF-16 with BOM is what Desktop writes; UTF-8 is accepted so a hand-made
    // file in a test does not need a wide encoder.
    internal static int ParsePortFile(byte[] raw)
    {
        if (raw == null || raw.Length == 0)
        {
            throw new InvalidOperationException("msmdsrv.port.txt is empty");
        }
        foreach (var enc in new[] { Encoding.Unicode, Encoding.BigEndianUnicode, new UTF8Encoding(true), Encoding.UTF8 })
        {
            string text;
            try
            {
                text = enc.GetString(raw);
            }
            catch (DecoderFallbackException)
            {
                continue;
            }
            var digits = text.Trim().Trim('\ufeff').Replace("\0", "", StringComparison.Ordinal).Trim();
            if (int.TryParse(digits, NumberStyles.None, CultureInfo.InvariantCulture, out var port)
                && port is >= 1 and <= 65535)
            {
                return port;
            }
        }
        throw new InvalidOperationException("msmdsrv.port.txt did not contain a port");
    }

    internal static string FirstLine(Exception e) => e.Message.Split('\n')[0].Trim();
}
