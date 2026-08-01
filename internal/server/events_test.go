package server_test

// The flow stream over real HTTP: a real pipeline runs, and the events it
// caused arrive on the wire as SSE frames while it happens.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	entra "github.com/calvinchengx/entra-emulator/emulator"
)

// sseEvent is one parsed `id:/event:/data:` frame.
type sseEvent struct {
	ID   string
	Kind string
	Data map[string]any
}

// openStream connects to the flow stream and returns a channel of frames plus
// a close func. Frames are parsed on a goroutine so the caller can trigger work
// and then wait for what it caused.
func (f *fixture) openStream(t *testing.T, query string) (<-chan sseEvent, func()) {
	t.Helper()
	ctx, cancelReq := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", f.fabric.URL+"/_emulator/events"+query, nil)
	if err != nil {
		cancelReq()
		t.Fatal(err)
	}
	resp, err := f.fabric.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancelReq()
		t.Fatalf("stream = %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	out := make(chan sseEvent, 256)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var cur sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				cur.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.Kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.Data)
			case line == "" && cur.Kind != "":
				out <- cur
				cur = sseEvent{}
			}
		}
	}()
	// Cancel the request, not just the body: an SSE handler blocked in select
	// only notices a closed body at its next write, which would make every
	// test here wait out a keepalive interval.
	// Force the connection shut rather than relying on the request context:
	// httptest.Server.Close waits for outstanding requests, and a handler
	// parked in select does not reliably observe a client hang-up until its
	// next write — which would make every test here wait out a keepalive.
	return out, func() { cancelReq(); f.fabric.CloseClientConnections() }
}

// awaitEvent reads frames until pred matches or the deadline passes.
func awaitEvent(t *testing.T, ch <-chan sseEvent, what string, pred func(sseEvent) bool) sseEvent {
	t.Helper()
	deadline := time.After(15 * time.Second)
	var seen []string
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before %s; saw %v", what, seen)
			}
			seen = append(seen, ev.Kind)
			if pred(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s; saw %v", what, seen)
		}
	}
}

func TestFlowStreamReportsRealDataMovement(t *testing.T) {
	f := newFixture(t)

	var ws struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]string{"displayName": "Flow"}, &ws), http.StatusCreated, "workspace")
	var lake struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]any{"displayName": "lake", "type": "Lakehouse"}, &lake), http.StatusCreated, "lakehouse")

	events, closeStream := f.openStream(t, "")
	defer closeStream()

	// A real ADLS upload, the way any storage client writes.
	storage := f.forgeToken(t, map[string]any{
		"clientId": entra.DaemonClientID, "audience": "https://storage.azure.com",
	})
	f.uploadToOneLake(t, storage, ws.ID, lake.ID, "Files/landing/orders.csv", "id,total\n1,10\n")

	ev := awaitEvent(t, events, "the upload's file event", func(e sseEvent) bool {
		return e.Kind == "file" && e.Data["path"] == "Files/landing/orders.csv"
	})
	if ev.Data["eventType"] != "Microsoft.Fabric.OneLake.FileCreated" {
		t.Fatalf("eventType = %v", ev.Data["eventType"])
	}
	if ev.Data["itemId"] != lake.ID || ev.Data["workspaceId"] != ws.ID {
		t.Fatalf("scoping = %+v", ev.Data)
	}
	if ev.ID == "" {
		t.Fatal("no SSE id: — EventSource could not resume")
	}
}

func TestFlowStreamDerivesTableVersionsFromAPipelineCopy(t *testing.T) {
	// The medallion-shaped case: a Copy activity writes a Delta table, and the
	// stream says which table reached which version with how many rows —
	// rather than a wall of Parquet part paths.
	f := newFixture(t)

	var ws struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]string{"displayName": "FlowCopy"}, &ws), http.StatusCreated, "workspace")
	var lake, pipe struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]any{"displayName": "lake", "type": "Lakehouse"}, &lake), http.StatusCreated, "lakehouse")
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]any{"displayName": "ingest", "type": "DataPipeline"}, &pipe), http.StatusCreated, "pipeline")

	storage := f.forgeToken(t, map[string]any{
		"clientId": entra.DaemonClientID, "audience": "https://storage.azure.com",
	})
	f.uploadToOneLake(t, storage, ws.ID, lake.ID, "Files/landing/customers.csv",
		"id,name\n1,ada\n2,grace\n3,edsger\n")

	lakeRef := map[string]any{"linkedService": map[string]any{"properties": map[string]any{
		"type": "Lakehouse", "typeProperties": map[string]any{"workspaceId": ws.ID, "artifactId": lake.ID}}}}
	def := map[string]any{"properties": map[string]any{"activities": []map[string]any{{
		"name": "IngestCustomers", "type": "Copy", "typeProperties": map[string]any{
			"source": map[string]any{"type": "DelimitedTextSource", "rootFolder": "Files",
				"folderPath": "landing", "fileName": "customers.csv", "datasetSettings": lakeRef},
			"sink": map[string]any{"type": "LakehouseTableSink", "tableActionOption": "Overwrite",
				"table": "bronze_customers", "datasetSettings": lakeRef},
		}}}}}
	raw, _ := json.Marshal(def)
	update := map[string]any{"definition": map[string]any{"parts": []map[string]string{{
		"path": "pipeline-content.json", "payload": base64.StdEncoding.EncodeToString(raw),
		"payloadType": "InlineBase64"}}}}
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+pipe.ID+"/updateDefinition",
		f.token, update, nil), http.StatusAccepted, "updateDefinition")

	// Only table events: the noise filter that makes this watchable.
	events, closeStream := f.openStream(t, "?kinds=table")
	defer closeStream()

	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+pipe.ID+
		"/jobs/instances?jobType=Pipeline", f.token, map[string]any{}, nil),
		http.StatusAccepted, "run pipeline")

	ev := awaitEvent(t, events, "the bronze table event", func(e sseEvent) bool {
		return e.Data["table"] == "Tables/bronze_customers"
	})
	if ev.Kind != "table" {
		t.Fatalf("the kinds filter let through a %q", ev.Kind)
	}
	if got, ok := ev.Data["rowsAdded"].(float64); !ok || got != 3 {
		t.Fatalf("rowsAdded = %v, want the 3 rows the CSV had", ev.Data["rowsAdded"])
	}
	if got, ok := ev.Data["filesAdded"].(float64); !ok || got < 1 {
		t.Fatalf("filesAdded = %v", ev.Data["filesAdded"])
	}
}

func TestFlowStreamReplaysWithSince(t *testing.T) {
	// A developer who starts watching after the run began still sees it.
	f := newFixture(t)
	var ws struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]string{"displayName": "FlowReplay"}, &ws), http.StatusCreated, "workspace")
	var lake struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]any{"displayName": "lake", "type": "Lakehouse"}, &lake), http.StatusCreated, "lakehouse")

	storage := f.forgeToken(t, map[string]any{
		"clientId": entra.DaemonClientID, "audience": "https://storage.azure.com",
	})
	// Written before anyone subscribes.
	f.uploadToOneLake(t, storage, ws.ID, lake.ID, "Files/early.csv", "x")

	events, closeStream := f.openStream(t, "?since=0")
	defer closeStream()
	first := awaitEvent(t, events, "the replayed event", func(e sseEvent) bool {
		return e.Data["path"] == "Files/early.csv"
	})

	// Reconnecting with the last id seen must not repeat it.
	events2, close2 := f.openStream(t, "?since="+first.ID)
	defer close2()
	f.uploadToOneLake(t, storage, ws.ID, lake.ID, "Files/late.csv", "y")
	ev := awaitEvent(t, events2, "the post-reconnect event", func(e sseEvent) bool {
		return e.Kind == "file"
	})
	if ev.Data["path"] == "Files/early.csv" {
		t.Fatal("since= replayed an event the client already had")
	}
}

func TestFileEventFiresAtFlushNotAtTheEmptyCreate(t *testing.T) {
	// The ADLS protocol writes in three steps: create the path, append the
	// bytes, flush. Announcing the create would mean a Reflex trigger ran — and
	// a flow event claimed a table landed — while the file was still empty.
	// Azure's own ADLS Gen2 raises BlobCreated on FlushWithClose for the same
	// reason.
	f := newFixture(t)
	var ws struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]string{"displayName": "FlushTiming"}, &ws), http.StatusCreated, "workspace")
	var lake struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]any{"displayName": "lake", "type": "Lakehouse"}, &lake), http.StatusCreated, "lakehouse")

	events, closeStream := f.openStream(t, "")
	defer closeStream()
	storage := f.forgeToken(t, map[string]any{
		"clientId": entra.DaemonClientID, "audience": "https://storage.azure.com",
	})

	const body = "id,name\n1,ada\n"
	base := "/" + ws.ID + "/" + lake.ID + "/Files/staged.csv"
	step := func(method, path string, payload []byte, want int, ctx string) {
		if resp := f.ol(t, method, path, storage, payload); resp.StatusCode != want {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s: %d, want %d — %s", ctx, resp.StatusCode, want, raw)
		}
	}

	step("PUT", base+"?resource=file", nil, http.StatusCreated, "create")
	step("PATCH", base+"?action=append&position=0", []byte(body), http.StatusAccepted, "append")
	// Nothing announced yet: the file exists but holds no data.
	select {
	case ev := <-events:
		t.Fatalf("announced %+v before the write was flushed", ev.Data)
	case <-time.After(300 * time.Millisecond):
	}

	step("PATCH", base+fmt.Sprintf("?action=flush&position=%d", len(body)), nil, http.StatusOK, "flush")
	ev := awaitEvent(t, events, "the flushed file", func(e sseEvent) bool {
		return e.Kind == "file" && e.Data["path"] == "Files/staged.csv"
	})
	if ev.Data["eventType"] != "Microsoft.Fabric.OneLake.FileCreated" {
		t.Fatalf("eventType = %v", ev.Data["eventType"])
	}
	// Exactly one announcement per file — not one per protocol step.
	select {
	case dup := <-events:
		t.Fatalf("a second event for one write: %+v", dup.Data)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestFlowStreamShowsAFailingPipelineAsItHappens(t *testing.T) {
	// The debugging story end to end: without touching queryactivityruns, a
	// watcher sees which activity failed, why, and that the job failed.
	f := newFixture(t)
	var ws struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]string{"displayName": "FlowFail"}, &ws), http.StatusCreated, "workspace")
	var pipe struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]any{"displayName": "breaks", "type": "DataPipeline"}, &pipe), http.StatusCreated, "pipeline")

	def := `{"properties":{"activities":[
		{"name":"Step1","type":"Wait","typeProperties":{"waitTimeInSeconds":1}},
		{"name":"Step2","type":"Fail","typeProperties":{"message":"the gold table is empty","errorCode":"DQ001"},
		 "dependsOn":[{"activity":"Step1","dependencyConditions":["Succeeded"]}]}]}}`
	update := map[string]any{"definition": map[string]any{"parts": []map[string]string{{
		"path": "pipeline-content.json", "payload": base64.StdEncoding.EncodeToString([]byte(def)),
		"payloadType": "InlineBase64"}}}}
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+pipe.ID+"/updateDefinition",
		f.token, update, nil), http.StatusAccepted, "updateDefinition")

	events, closeStream := f.openStream(t, "?kinds=job,activity")
	defer closeStream()

	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+pipe.ID+
		"/jobs/instances?jobType=Pipeline", f.token, map[string]any{}, nil),
		http.StatusAccepted, "run pipeline")

	started := awaitEvent(t, events, "the job starting", func(e sseEvent) bool {
		return e.Kind == "job" && e.Data["status"] == "Started"
	})
	jobID, _ := started.Data["jobId"].(string)
	if jobID == "" {
		t.Fatalf("job event carries no jobId: %+v", started.Data)
	}

	ok := awaitEvent(t, events, "the succeeding activity", func(e sseEvent) bool {
		return e.Kind == "activity" && e.Data["activityName"] == "Step1"
	})
	if ok.Data["status"] != "Succeeded" || ok.Data["activityType"] != "Wait" {
		t.Fatalf("Step1 = %+v", ok.Data)
	}

	bad := awaitEvent(t, events, "the failing activity", func(e sseEvent) bool {
		return e.Kind == "activity" && e.Data["activityName"] == "Step2"
	})
	if bad.Data["status"] != "Failed" {
		t.Fatalf("Step2 = %+v", bad.Data)
	}
	if msg, _ := bad.Data["error"].(string); !strings.Contains(msg, "the gold table is empty") {
		t.Fatalf("the failure reached the stream without its message: %+v", bad.Data)
	}
	if bad.Data["jobId"] != jobID {
		t.Fatalf("activity not correlated to its job: %+v", bad.Data)
	}

	done := awaitEvent(t, events, "the job failing", func(e sseEvent) bool {
		return e.Kind == "job" && e.Data["status"] == "Failed"
	})
	if done.Data["failureReason"] == "" || done.Data["jobId"] != jobID {
		t.Fatalf("terminal job event = %+v", done.Data)
	}
}

func TestNotebookCellWritesAreAttributedOnTheStream(t *testing.T) {
	// A notebook runtime executes cells one at a time, so it always knows
	// which one is running and says so on the request. The flow stream reuses
	// exactly that — no inference from user code.
	f := newFixture(t)
	var ws struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]string{"displayName": "FlowAttr"}, &ws), http.StatusCreated, "workspace")
	var lake struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]any{"displayName": "lake", "type": "Lakehouse"}, &lake), http.StatusCreated, "lakehouse")

	events, closeStream := f.openStream(t, "?kinds=file")
	defer closeStream()
	storage := f.forgeToken(t, map[string]any{
		"clientId": entra.DaemonClientID, "audience": "https://storage.azure.com",
	})

	const body = "id\n1\n"
	base := "/" + ws.ID + "/" + lake.ID + "/Files/from-cell.csv"
	// Cell 0 deliberately: the index that a non-pointer field could not tell
	// apart from "no cell".
	hdr := map[string]string{"x-ms-fabric-job-id": "job-abc", "x-ms-fabric-cell-index": "0"}
	f.olWithHeaders(t, "PUT", base+"?resource=file", storage, nil, hdr, http.StatusCreated)
	f.olWithHeaders(t, "PATCH", base+"?action=append&position=0", storage, []byte(body), hdr, http.StatusAccepted)
	f.olWithHeaders(t, "PATCH", base+fmt.Sprintf("?action=flush&position=%d", len(body)), storage, nil, hdr, http.StatusOK)

	ev := awaitEvent(t, events, "the attributed write", func(e sseEvent) bool {
		return e.Data["path"] == "Files/from-cell.csv"
	})
	attr, ok := ev.Data["attribution"].(map[string]any)
	if !ok {
		t.Fatalf("no attribution on %+v", ev.Data)
	}
	if attr["jobId"] != "job-abc" {
		t.Fatalf("attribution = %+v", attr)
	}
	cell, ok := attr["cellIndex"].(float64)
	if !ok || cell != 0 {
		t.Fatalf("cellIndex = %v, want 0 present", attr["cellIndex"])
	}
}

// olWithHeaders is f.ol with extra request headers.
func (f *fixture) olWithHeaders(t *testing.T, method, path, token string, body []byte,
	headers map[string]string, want int) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, f.fabric.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "onelake.dfs.fabric.microsoft.com"
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.fabric.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s = %d, want %d — %s", method, path, resp.StatusCode, want, raw)
	}
}
