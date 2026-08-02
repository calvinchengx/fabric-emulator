using System;
using Microsoft.AnalysisServices.AdomdClient;

class Probe {
    static int Main(string[] args) {
        Console.WriteLine($"adomd on {Environment.OSVersion}/{System.Runtime.InteropServices.RuntimeInformation.OSArchitecture}");
        // Token via the connection string, which is what a headless client does
        // and avoids depending on an API surface that moves between versions.
        foreach (var cs in new[] {
            "Data Source=powerbi://host.docker.internal:18080/v1.0/myorg/ws;User ID=;Password=dummy-token;",
            "Data Source=powerbi://host.docker.internal:18080/v1.0/myorg/ws;Password=dummy-token;",
            "Data Source=powerbi://host.docker.internal:18080/v1.0/myorg/ws;Integrated Security=ClaimsToken;Password=dummy-token;",
        }) {
            try {
                using var c = new AdomdConnection(cs);
                c.Open();
                Console.WriteLine($"  OPENED  {cs}");
                c.Close();
            } catch (Exception e) {
                Console.WriteLine($"  {e.GetType().Name}: {e.Message.Split('\n')[0]}");
            }
        }
        return 0;
    }
}
