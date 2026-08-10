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
                // PAST Open() — the whole point of getting the connection up.
                // Until the client is ASKED for query traffic it sends none, so
                // the envelopes that price the rowset serialiser never appear
                // and the `L` in docs/24 stays an estimate. Each probe below is
                // wrapped separately: the first one to fail must not hide the
                // others, because they exercise DIFFERENT surfaces (DAX query
                // vs schema Discover) and we want both answers from one run.
                RunProbe($"{label}/dax", () => {
                    using var cmd = c.CreateCommand();
                    cmd.CommandText = "EVALUATE ROW(\"x\", 1)";
                    using var r = cmd.ExecuteReader();
                    while (r.Read()) { }
                });
                // Discover, the metadata half. sempy's list_tables/list_measures
                // and semantic-link-labs' TOM both live here, and per the
                // measured API defaults they use XMLA rather than REST — so
                // this is the surface the SemPy demand actually needs.
                RunProbe($"{label}/schema", () => {
                    var ds = c.GetSchemaDataSet("MDSCHEMA_MEASURES", null);
                    Console.WriteLine($"SCHEMA {label} :: rows={ds.Tables[0].Rows.Count}");
                });
                c.Close();
            } catch (Exception e) {
                var first = e.Message.Split('\n')[0].Trim();
                Console.WriteLine($"CASE {label} :: {e.GetType().Name} :: {first}");
                // The exception carries the name of the method that threw. That
                // is strictly more than the message says and costs one run to
                // collect — where screening candidate payloads costs one run
                // PER GUESS. Screen 5 ended on an IndexOutOfRangeException whose
                // message ("Index was outside the bounds of the array") names
                // nothing at all; the stack names the parser.
                //
                // Skipped for NotSupportedException: the Windows-only Data
                // Source forms are settled, and frames for them are noise.
                if (!(e is NotSupportedException)) {
                    for (var x = e; x != null; x = x.InnerException) {
                        if (!ReferenceEquals(x, e))
                            Console.WriteLine($"  INNER {label} :: {x.GetType().Name} :: " +
                                $"{x.Message.Split('\n')[0].Trim()}");
                        if (x.TargetSite != null)
                            Console.WriteLine($"  THREW {label} :: " +
                                $"{x.TargetSite.DeclaringType}::{x.TargetSite.Name}");
                        foreach (var frame in (x.StackTrace ?? "").Split('\n')) {
                            var f = frame.Trim();
                            if (f.Length > 0) Console.WriteLine($"  FRAME {label} :: {f}");
                        }
                    }
                }
            }
        }
        return 0;
    }

    // Run one post-Open probe and REPORT rather than throw. A probe that killed
    // the run would let the first failure hide every later surface, and these
    // exercise different ones (DAX query vs schema Discover). Prints the
    // throwing method for the same reason the connect path does: the method
    // name is strictly more than the message says, and it costs no extra run.
    static void RunProbe(string label, Action body) {
        try {
            body();
            Console.WriteLine($"PROBE {label} :: OK");
        } catch (Exception e) {
            Console.WriteLine($"PROBE {label} :: {e.GetType().Name} :: "
                              + e.Message.Split('\n')[0].Trim());
            for (var x = e; x != null; x = x.InnerException) {
                if (x.TargetSite != null)
                    Console.WriteLine($"  PTHREW {label} :: "
                                      + $"{x.TargetSite.DeclaringType}::{x.TargetSite.Name}");
            }
        }
    }
}
