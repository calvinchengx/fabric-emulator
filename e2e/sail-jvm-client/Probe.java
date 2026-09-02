/*
 * Does a real JVM Spark Connect client work against Sail?
 *
 * This is the whole premise behind running a Databricks JAR task on the DEFAULT
 * engine: Sail is a Connect SERVER, so the JVM half is a client and needs no
 * cluster. If the handshake or the plans do not survive, the idea dies here.
 *
 * Every case prints one machine-readable line and swallows its own failure, so
 * ONE run reports the whole boundary rather than stopping at the first refusal.
 * run.py decides what the results mean; this program only observes. The exit
 * code is taken from --exit-with so the driver can prove that a jar's exit code
 * reaches the caller, which is the signal the activity ultimately reports on.
 */
import java.util.Arrays;
import java.util.List;

import org.apache.spark.api.java.function.MapFunction;
import org.apache.spark.sql.Encoders;
import org.apache.spark.sql.SparkSession;
import org.apache.spark.sql.api.java.UDF1;
import org.apache.spark.sql.types.DataTypes;

public class Probe {

    /** One line per case: CASE <id> <ran|refused> :: <detail>. */
    static void observe(String id, Runnable body) {
        try {
            body.run();
            System.out.println("CASE " + id + " ran :: ok");
        } catch (Throwable t) {
            String detail = String.valueOf(t.getMessage() == null ? t.toString() : t.getMessage());
            detail = detail.replace('\n', ' ').replace('\r', ' ').trim();
            if (detail.length() > 200) {
                detail = detail.substring(0, 200);
            }
            System.out.println("CASE " + id + " refused :: " + detail);
        }
    }

    public static void main(String[] argv) {
        int exitWith = 0;
        String remote = System.getenv().getOrDefault("SPARK_REMOTE", "sc://localhost:50051");
        String artifact = null, sentinel = null, warehouse = null;
        for (int i = 0; i < argv.length - 1; i++) {
            if (argv[i].equals("--exit-with")) exitWith = Integer.parseInt(argv[i + 1]);
            if (argv[i].equals("--artifact")) artifact = argv[i + 1];
            if (argv[i].equals("--sentinel")) sentinel = argv[i + 1];
            if (argv[i].equals("--warehouse")) warehouse = argv[i + 1];
        }
        final String jar = artifact, expected = sentinel, out = warehouse + "/probe_parquet";

        final SparkSession spark = SparkSession.builder().remote(remote).getOrCreate();

        // The five that must WORK, or a jar cannot do useful work here.
        observe("handshake_sql", () -> {
            int v = spark.sql("select 1 as a").collectAsList().get(0).getInt(0);
            if (v != 1) throw new RuntimeException("expected 1, got " + v);
        });
        observe("dataframe_ops", () -> {
            long n = spark.range(10).filter("id % 2 = 0").count();
            if (n != 5) throw new RuntimeException("expected 5, got " + n);
        });
        observe("parquet_roundtrip", () -> {
            spark.range(5).write().mode("overwrite").parquet(out);
            long n = spark.read().parquet(out).count();
            if (n != 5) throw new RuntimeException("wrote 5, read " + n);
        });
        observe("sql_ddl_view", () -> {
            spark.sql("create or replace temporary view v as select 1 as a").count();
            if (spark.sql("select * from v").count() != 1) throw new RuntimeException("view empty");
        });
        observe("args_passed", () -> {
            if (expected == null || !Arrays.asList(argv).contains(expected)) {
                throw new RuntimeException("sentinel missing from " + Arrays.toString(argv));
            }
        });

        // The four that must REFUSE. Each needs a JVM on the SERVER side, and
        // Sail is Rust — so a pass here means the boundary moved, not that the
        // probe got luckier.
        observe("typed_dataset_map", () -> {
            List<Long> got = spark.range(3)
                    .map((MapFunction<Long, Long>) v -> v * 2, Encoders.LONG()).collectAsList();
            if (got.isEmpty()) throw new RuntimeException("empty result");
        });
        observe("spark_context", () -> {
            Object sc = spark.sparkContext();
            if (sc == null) throw new RuntimeException("null SparkContext");
        });
        observe("scala_udf", () -> {
            spark.udf().register("twice", (UDF1<Long, Long>) v -> v * 2, DataTypes.LongType);
            spark.sql("select twice(21) as t").collectAsList();
        });
        observe("add_artifact", () -> {
            spark.addArtifact(jar);
        });

        System.out.println("CASE exit_code ran :: exiting " + exitWith);
        System.exit(exitWith);
    }
}
