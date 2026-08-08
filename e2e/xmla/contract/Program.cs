using System;
using System.Linq;
using System.Reflection;
using System.Runtime.Serialization;

// Read the DataContract that GetMwcToken deserialises its reply into.
// docs/32 established the routing reply's shape this way rather than by
// screening; the same method should settle generateastoken's response.
class Read {
    // CONTRACT_FILTER narrows both sweeps; empty means the token exchange,
    // which is what this was first written to answer.
    static readonly string[] Filters =
        (Environment.GetEnvironmentVariable("CONTRACT_FILTER") ?? "").Trim() is { Length: > 0 } f
            ? new[] { f } : new[] { "Token", "Mwc", "GenerateAs" };

    static bool Matches(string name) =>
        Filters.Any(x => name.Contains(x, StringComparison.OrdinalIgnoreCase));

    static void Main() {
        var asm = typeof(Microsoft.AnalysisServices.AdomdClient.AdomdConnection).Assembly;
        Console.WriteLine("ASSEMBLY " + asm.GetName().Name + " " + asm.GetName().Version);

        const BindingFlags ALL = BindingFlags.Public | BindingFlags.NonPublic
                               | BindingFlags.Instance | BindingFlags.Static;

        // 1. Find GetMwcToken and report its signature — the return type (or an
        //    out/local) names the contract we must satisfy.
        foreach (var t in asm.GetTypes()) {
            MethodInfo[] ms;
            try { ms = t.GetMethods(ALL); } catch { continue; }
            foreach (var m in ms) {
                if (!Matches(m.Name)) continue;
                Console.WriteLine($"METHOD {t.FullName}::{m.Name}");
                Console.WriteLine($"  returns {m.ReturnType.FullName}");
                foreach (var p in m.GetParameters())
                    Console.WriteLine($"  param  {p.ParameterType.FullName} {p.Name}"
                                      + (p.IsOut ? " [out]" : ""));
            }
        }

        // 2. Dump every [DataContract] whose name suggests the token exchange.
        //    Member names come from [DataMember(Name=...)], which is what
        //    DataContractJsonSerializer matches on.
        Console.WriteLine("---- candidate contracts ----");
        foreach (var t in asm.GetTypes()) {
            if (t.GetCustomAttribute<DataContractAttribute>() == null) continue;
            var n = t.FullName ?? "";
            if (!Matches(n)) continue;
            Console.WriteLine("CONTRACT " + n);
            foreach (var f in t.GetFields(ALL)) {
                var dm = f.GetCustomAttribute<DataMemberAttribute>();
                if (dm != null)
                    Console.WriteLine($"    field  {dm.Name ?? f.Name} : {f.FieldType.Name}");
            }
            foreach (var p in t.GetProperties(ALL)) {
                var dm = p.GetCustomAttribute<DataMemberAttribute>();
                if (dm != null)
                    Console.WriteLine($"    prop   {dm.Name ?? p.Name} : {p.PropertyType.Name}");
            }
            if (t.IsEnum)
                Console.WriteLine("    enum values: " + string.Join(" | ", Enum.GetNames(t)));
        }
    }
}
