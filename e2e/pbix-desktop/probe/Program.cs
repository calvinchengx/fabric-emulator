using System;
using System.Globalization;
using System.Text;
using Microsoft.AnalysisServices.AdomdClient;

// Query the model Power BI DESKTOP loaded, not a reimplementation of it.
//
// Desktop hosts a real Analysis Services instance (msmdsrv.exe) on a loopback
// port; this is how Tabular Editor and DAX Studio attach, and it is the only
// way to make Desktop answer a question without a human clicking. So the claim
// this suite can support — "Desktop read the file and evaluated the DAX" — is
// exactly as strong as this connection is real.
//
// One machine-readable line per fact, like e2e/xmla's probe: the harness reads
// outcomes rather than pattern-matching prose.
//
//     STAGE connect :: OK | <ExceptionType> :: <first line>
//     ROW <groupValue> <measure>=<invariant double>
//     STAGE query :: OK | <ExceptionType> :: <first line>
class DesktopProbe {
    static int Main(string[] args) {
        var port = Environment.GetEnvironmentVariable("PBI_PORT");
        var dax  = Environment.GetEnvironmentVariable("PBI_DAX");
        if (string.IsNullOrEmpty(port) || string.IsNullOrEmpty(dax)) {
            Console.Error.WriteLine("PBI_PORT and PBI_DAX are required");
            return 2;
        }

        // localhost:<port>, the bare form. e2e/xmla measured that form as
        // Windows-only on .NET Core — which is fine and is the point: this
        // probe only ever runs on Windows, and a LOCAL msmdsrv is exactly the
        // case that form exists for.
        var cs = $"Data Source=localhost:{port};";
        AdomdConnection conn;
        try {
            conn = new AdomdConnection(cs);
            conn.Open();
            Console.WriteLine("STAGE connect :: OK");
        } catch (Exception e) {
            Console.WriteLine($"STAGE connect :: {e.GetType().Name} :: {e.Message.Split('\n')[0].Trim()}");
            return 1;
        }

        try {
            using var cmd = conn.CreateCommand();
            cmd.CommandText = dax;
            using var r = cmd.ExecuteReader();
            while (r.Read()) {
                var sb = new StringBuilder("ROW");
                for (int i = 0; i < r.FieldCount; i++) {
                    var name = r.GetName(i);
                    var val = r.IsDBNull(i) ? "" : r.GetValue(i);
                    // Invariant culture with R: the harness parses these back
                    // as doubles and compares to our emulator. A runner with a
                    // comma decimal separator would otherwise turn 101.72 into
                    // a parse error at best and 10172 at worst.
                    var s = val is double d
                        ? d.ToString("R", CultureInfo.InvariantCulture)
                        : Convert.ToString(val, CultureInfo.InvariantCulture);
                    sb.Append($" {name}={s}");
                }
                Console.WriteLine(sb.ToString());
            }
            Console.WriteLine("STAGE query :: OK");
        } catch (Exception e) {
            Console.WriteLine($"STAGE query :: {e.GetType().Name} :: {e.Message.Split('\n')[0].Trim()}");
            return 1;
        } finally {
            conn.Close();
        }
        return 0;
    }
}
