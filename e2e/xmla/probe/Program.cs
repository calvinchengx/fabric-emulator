using System;
using Microsoft.AnalysisServices.AdomdClient;

// Microsoft's own ADOMD.NET client, driven headlessly at a host WE name.
//
// One machine-readable line per case, so run.py asserts on outcomes rather than
// pattern-matching prose:
//
//     PLATFORM <platform>/<arch>
//     CASE <label> :: OPENED
//     CASE <label> :: <ExceptionType> :: <first line of the message>
//
// Every case is expected to FAIL: the listener on the other side is a capture
// stub that refuses the client's first call. What is under test is not whether
// a connection succeeds but WHERE the bytes went and WHAT was in them — which
// run.py reads off the listener — plus which failures are the client refusing
// locally (the Windows-only Data Source forms) rather than the server refusing.
class Probe {
    static int Main(string[] args) {
        var target = Environment.GetEnvironmentVariable("XMLA_TARGET");
        if (string.IsNullOrEmpty(target)) {
            Console.Error.WriteLine("XMLA_TARGET is required (host:port)");
            return 2;
        }
        var token = Environment.GetEnvironmentVariable("XMLA_TOKEN") ?? "dummy-token";

        Console.WriteLine($"PLATFORM {Environment.OSVersion.Platform}/" +
            $"{System.Runtime.InteropServices.RuntimeInformation.OSArchitecture}");

        // Token via the connection string, which is what a headless client does
        // and avoids depending on an API surface that moves between versions.
        var cases = new (string Label, string ConnectionString)[] {
            ("powerbi-userid",
                $"Data Source=powerbi://{target}/v1.0/myorg/ws;User ID=;Password={token};"),
            ("powerbi-bare",
                $"Data Source=powerbi://{target}/v1.0/myorg/ws;Password={token};"),
            ("powerbi-claimstoken",
                $"Data Source=powerbi://{target}/v1.0/myorg/ws;Integrated Security=ClaimsToken;Password={token};"),
            // The two forms a naive port reaches for first. Both are Windows-only
            // on .NET Core; asserting that keeps the constraint checkable instead
            // of a claim in prose, and tells us if a later ADOMD.NET lifts it.
            ("http-xmla",
                $"Data Source=https://{target}/xmla;Password={token};"),
            ("hostport",
                $"Data Source={target};Password={token};"),
        };

        foreach (var (label, cs) in cases) {
            try {
                using var c = new AdomdConnection(cs);
                c.Open();
                Console.WriteLine($"CASE {label} :: OPENED");
                c.Close();
            } catch (Exception e) {
                var first = e.Message.Split('\n')[0].Trim();
                Console.WriteLine($"CASE {label} :: {e.GetType().Name} :: {first}");
            }
        }
        return 0;
    }
}
