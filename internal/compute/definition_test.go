package compute

import (
	"encoding/base64"
	"reflect"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func part(path, body string) store.DefinitionPart {
	return store.DefinitionPart{Path: path, Payload: base64.StdEncoding.EncodeToString([]byte(body))}
}

func TestNotebookBinding(t *testing.T) {
	source := []byte(`# METADATA
# META {
# META "dependencies": {
# META   "lakehouse": {"default_lakehouse":"lh", "default_lakehouse_name":"sales", "default_lakehouse_workspace_id":"ws"},
# META   "environment": {"environmentId":"env", "workspaceId":"env-ws"}
# META }}
# META {"cellMetadata":{"collapsed":false}}
`)
	want := Binding{WorkspaceID: "ws", LakehouseID: "lh", LakehouseName: "sales", EnvironmentID: "env", EnvironmentWorkspaceID: "env-ws"}
	if got := NotebookBinding(source); got != want {
		t.Fatalf("binding = %+v, want %+v", got, want)
	}
	if got := NotebookBinding([]byte("print('x')")); got != (Binding{}) {
		t.Fatalf("missing metadata = %+v", got)
	}
	if got := NotebookBinding([]byte("# META nope")); got != (Binding{}) {
		t.Fatalf("malformed metadata = %+v", got)
	}
}

func TestParseEnvironment(t *testing.T) {
	got, err := ParseEnvironment([]store.DefinitionPart{
		part("requirements.txt", "pandas==2.2.3\n# comment\ndelta-spark>=3\n"),
		part("Setting/Sparkcompute.json", `{"sparkProperties":{"spark.sql.shuffle.partitions":8},"libraries":["extra.jar","pyarrow==17"]}`),
		part("Libraries/local.jar", "bytes"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.PythonPackages, []string{"delta-spark>=3", "pandas==2.2.3", "pyarrow==17"}) {
		t.Fatalf("packages = %#v", got.PythonPackages)
	}
	if !reflect.DeepEqual(got.JARs, []string{"Libraries/local.jar", "extra.jar"}) {
		t.Fatalf("jars = %#v", got.JARs)
	}
	if got.SparkConfig["spark.sql.shuffle.partitions"] != "8" {
		t.Fatalf("config = %#v", got.SparkConfig)
	}
}

func TestParseSparkJobV1AndFailures(t *testing.T) {
	job, binding, err := ParseSparkJob([]store.DefinitionPart{
		part("SparkJobDefinitionV1.json", `{"executableFile":"main.py","arguments":["--date","2026-07-30"],"libraries":["lib.py"],"defaultLakehouseArtifactId":"lh","defaultLakehouseWorkspaceId":"ws","environmentArtifactId":"env","environmentWorkspaceId":"env-ws"}`),
		part("main.py", "print('ok')"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.MainFile != "main.py" || job.Source != "print('ok')" || len(job.Arguments) != 2 {
		t.Fatalf("job = %+v", job)
	}
	if binding != (Binding{WorkspaceID: "ws", LakehouseID: "lh", EnvironmentID: "env", EnvironmentWorkspaceID: "env-ws"}) {
		t.Fatalf("binding = %+v", binding)
	}
	if _, _, err := ParseSparkJob([]store.DefinitionPart{part("main.py", "x=1")}); err == nil {
		t.Fatal("missing config succeeded")
	}
	if _, _, err := ParseSparkJob([]store.DefinitionPart{part("SparkJobDefinitionV1.json", `{"executableFile":"gone.py"}`)}); err == nil {
		t.Fatal("missing main succeeded")
	}
}

func TestDefinitionDecodeError(t *testing.T) {
	_, err := ParseEnvironment([]store.DefinitionPart{{Path: "requirements.txt", Payload: "%%%"}})
	if err == nil {
		t.Fatal("invalid base64 succeeded")
	}
}
