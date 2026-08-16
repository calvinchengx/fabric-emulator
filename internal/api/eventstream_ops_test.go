package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

func opEvents(values ...string) []struct {
	Key   string `json:"key"`
	Value string `json:"value"`
} {
	out := make([]struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}, len(values))
	for i, v := range values {
		out[i].Value = v
	}
	return out
}

func TestEventstreamBindOperatorValidation(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	es, _ := seedEventstream(t, a, ws.ID)
	nb := do(a.typedCreate("Notebook"), admin, http.MethodPost,
		`{"displayName":"nb"}`, map[string]string{"wid": ws.ID})
	var notebook struct{ ID string }
	_ = json.Unmarshal(nb.Body.Bytes(), &notebook)
	path := map[string]string{"wid": ws.ID, "iid": es}

	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"malformed", `{`, "InvalidRequest", http.StatusBadRequest},
		{"empty type", `{"type":""}`, "EventstreamOperatorNotSupported", http.StatusBadRequest},
		{"filter no condition", `{"type":"Filter"}`, "EventstreamOperatorNotSupported", http.StatusBadRequest},
		{"filter no field", `{"type":"Filter","condition":{"op":"eq"}}`, "EventstreamOperatorNotSupported", http.StatusBadRequest},
		{"filter no op", `{"type":"Filter","condition":{"field":"n"}}`, "EventstreamOperatorNotSupported", http.StatusBadRequest},
		{"filter bad op", `{"type":"Filter","condition":{"field":"n","op":"like"}}`, "EventstreamOperatorNotSupported", http.StatusBadRequest},
		{"groupby no aggs", `{"type":"GroupBy","keys":["src"]}`, "EventstreamOperatorNotSupported", http.StatusBadRequest},
		{"groupby no as", `{"type":"GroupBy","aggregates":[{"fn":"count"}]}`, "EventstreamOperatorNotSupported", http.StatusBadRequest},
		{"sum no field", `{"type":"GroupBy","aggregates":[{"fn":"sum","as":"t"}]}`, "EventstreamOperatorNotSupported", http.StatusBadRequest},
		{"bad agg", `{"type":"GroupBy","aggregates":[{"fn":"median","as":"m"}]}`, "EventstreamOperatorNotSupported", http.StatusBadRequest},
		{"window no duration", `{"type":"Window","kind":"tumbling"}`, "EventstreamOperatorNotSupported", http.StatusBadRequest},
		{"window zero", `{"type":"Window","kind":"tumbling","duration":"0s"}`, "EventstreamOperatorNotSupported", http.StatusBadRequest},
		{"window sliding", `{"type":"Window","kind":"sliding","duration":"1m"}`, "EventstreamOperatorNotSupported", http.StatusBadRequest},
		{"union", `{"type":"Union"}`, "EventstreamOperatorNotSupported", http.StatusBadRequest},
		{"expand", `{"type":"Expand"}`, "EventstreamOperatorNotSupported", http.StatusBadRequest},
	} {
		got := do(a.bindEventstreamOperator, admin, http.MethodPost, tc.body, path)
		if got.Code != tc.status || errorCode(t, got) != tc.code {
			t.Fatalf("%s: %d %s (code %s)", tc.name, got.Code, got.Body.Bytes(), errorCode(t, got))
		}
	}

	wrong := do(a.bindEventstreamOperator, admin, http.MethodPost,
		`{"type":"Filter","condition":{"field":"n","op":"eq","value":1}}`,
		map[string]string{"wid": ws.ID, "iid": notebook.ID})
	if wrong.Code != http.StatusNotFound || errorCode(t, wrong) != "ItemNotFound" {
		t.Fatalf("notebook bind: %d %s", wrong.Code, wrong.Body.Bytes())
	}
}

func TestEventstreamOperatorRBAC(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	es, _ := seedEventstream(t, a, ws.ID)
	path := map[string]string{"wid": ws.ID, "iid": es}
	body := `{"type":"Filter","condition":{"field":"n","op":"eq","value":1}}`

	if got := do(a.bindEventstreamOperator, viewer, http.MethodPost, body, path); got.Code != http.StatusForbidden {
		t.Fatalf("viewer bind: %d %s", got.Code, got.Body.Bytes())
	}
	if got := do(a.bindEventstreamOperator, nobody, http.MethodPost, body, path); got.Code != http.StatusForbidden {
		t.Fatalf("nobody bind: %d", got.Code)
	}
	empty := do(a.listEventstreamOperators, viewer, http.MethodGet, "", path)
	if empty.Code != http.StatusOK {
		t.Fatalf("viewer list: %d %s", empty.Code, empty.Body.Bytes())
	}
	var list struct{ Value []eventstreamOperator }
	if err := json.Unmarshal(empty.Body.Bytes(), &list); err != nil || list.Value == nil || len(list.Value) != 0 {
		t.Fatalf("empty list: %s", empty.Body.Bytes())
	}
	if got := do(a.listEventstreamOperators, nobody, http.MethodGet, "", path); got.Code != http.StatusForbidden {
		t.Fatalf("nobody list: %d", got.Code)
	}
}

func TestEventstreamOperatorPropertiesOnGet(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	es, _ := seedEventstream(t, a, ws.ID)
	path := map[string]string{"wid": ws.ID, "iid": es}
	if got := do(a.bindEventstreamOperator, admin, http.MethodPost,
		`{"type":"Filter","condition":{"field":"n","op":"eq","value":1}}`, path); got.Code != http.StatusCreated {
		t.Fatalf("bind: %d %s", got.Code, got.Body.Bytes())
	}
	listed := do(a.listEventstreamOperators, admin, http.MethodGet, "", path)
	var list struct{ Value []eventstreamOperator }
	_ = json.Unmarshal(listed.Body.Bytes(), &list)
	if listed.Code != http.StatusOK || len(list.Value) != 1 || list.Value[0].Type != "Filter" {
		t.Fatalf("list: %d %+v", listed.Code, list.Value)
	}
	gotItem := do(a.typedGet("Eventstream"), admin, http.MethodGet, "", path)
	var item struct {
		Properties struct {
			Operators []eventstreamOperator
		}
	}
	if err := json.Unmarshal(gotItem.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if len(item.Properties.Operators) != 1 || item.Properties.Operators[0].Type != "Filter" {
		t.Fatalf("GET omitted operators: %s", gotItem.Body.Bytes())
	}
}

func TestEventstreamPropertiesClosedStore(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	es, _ := seedEventstream(t, a, ws.ID)
	it, err := st.GetItem(ws.ID, es)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if got := a.eventstreamProperties(it); got != nil {
		t.Fatalf("closed store properties: %v", got)
	}
}

func TestEventstreamFilterOps(t *testing.T) {
	base := opEvents(`{"n":1,"s":"ab"}`, `{"n":2,"s":"cd"}`, `{"n":3,"s":"ab"}`)
	for _, tc := range []struct {
		name string
		cond eventstreamFilterCond
		want int
	}{
		{"eq", eventstreamFilterCond{Field: "n", Op: "eq", Value: 2}, 1},
		{"ne", eventstreamFilterCond{Field: "n", Op: "ne", Value: 2}, 2},
		{"gt", eventstreamFilterCond{Field: "n", Op: "gt", Value: 1}, 2},
		{"lt", eventstreamFilterCond{Field: "n", Op: "lt", Value: 3}, 2},
		{"lte", eventstreamFilterCond{Field: "n", Op: "lte", Value: 1}, 1},
		{"contains", eventstreamFilterCond{Field: "s", Op: "contains", Value: "a"}, 2},
		{"exists yes", eventstreamFilterCond{Field: "n", Op: "exists"}, 3},
		{"exists no", eventstreamFilterCond{Field: "missing", Op: "exists"}, 0},
		{"missing field", eventstreamFilterCond{Field: "missing", Op: "eq", Value: 1}, 0},
		{"string gt", eventstreamFilterCond{Field: "s", Op: "gt", Value: "b"}, 1},
		{"string gte", eventstreamFilterCond{Field: "s", Op: "gte", Value: "cd"}, 1},
		{"string lt", eventstreamFilterCond{Field: "s", Op: "lt", Value: "b"}, 2},
		{"string lte", eventstreamFilterCond{Field: "s", Op: "lte", Value: "ab"}, 2},
	} {
		got, err := applyFilter(eventstreamOperator{Type: "Filter", Condition: &tc.cond}, base)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Fatalf("%s: kept %d, want %d", tc.name, len(got), tc.want)
		}
	}
	nonJSON, err := applyFilter(eventstreamOperator{
		Type: "Filter", Condition: &eventstreamFilterCond{Field: "value", Op: "contains", Value: "not"},
	}, opEvents(`not-json`))
	if err != nil || len(nonJSON) != 1 {
		t.Fatalf("non-json: %v %+v", err, nonJSON)
	}
}

func TestEventstreamGroupByAggregates(t *testing.T) {
	got, err := applyGroupBy(eventstreamOperator{
		Type: "GroupBy",
		Keys: []string{"src"},
		Aggregates: []eventstreamAggregate{
			{Fn: "count", As: "n"},
			{Fn: "sum", Field: "v", As: "sum"},
			{Fn: "min", Field: "v", As: "min"},
			{Fn: "max", Field: "v", As: "max"},
			{Fn: "avg", Field: "v", As: "avg"},
		},
	}, opEvents(`{"src":"a","v":6}`, `{"src":"a","v":2}`, `{"src":"b","v":4}`, `{"src":"b","x":"skip"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("groups = %d: %+v", len(got), got)
	}
	var aRow, bRow map[string]any
	for _, ev := range got {
		var obj map[string]any
		_ = json.Unmarshal([]byte(ev.Value), &obj)
		switch obj["src"] {
		case "a":
			aRow = obj
		case "b":
			bRow = obj
		}
	}
	if asFloatMust(t, aRow["n"]) != 2 || asFloatMust(t, aRow["sum"]) != 8 ||
		asFloatMust(t, aRow["min"]) != 2 || asFloatMust(t, aRow["max"]) != 6 ||
		asFloatMust(t, aRow["avg"]) != 4 {
		t.Fatalf("a aggs: %v", aRow)
	}
	if asFloatMust(t, bRow["n"]) != 2 || asFloatMust(t, bRow["sum"]) != 4 {
		t.Fatalf("b skipped non-numeric: %v", bRow)
	}
}

func asFloatMust(t *testing.T, v any) float64 {
	t.Helper()
	f, ok := asFloat(v)
	if !ok {
		t.Fatalf("not a float: %T %v", v, v)
	}
	return f
}

func TestEventstreamWindowTimeFallbacks(t *testing.T) {
	op := eventstreamOperator{Type: "Window", Kind: "tumbling", Duration: "1h", On: "when"}
	got, err := applyWindow(op, opEvents(
		`{"when":"2026-01-01T00:10:00Z","n":1}`,
		`{"when":"nope","ts":"2026-01-01T01:10:00Z","n":2}`,
		`{"timestamp":"2026-01-01T02:10:00.123Z","n":3}`,
		`{"time":1770000000000,"n":4}`,
		`{"n":5}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("len=%d", len(got))
	}
	starts := make([]string, 0, 5)
	for _, ev := range got {
		var obj map[string]any
		_ = json.Unmarshal([]byte(ev.Value), &obj)
		s, _ := obj["_window_start"].(string)
		starts = append(starts, s)
	}
	if starts[0] != "2026-01-01T00:00:00Z" || starts[1] != "2026-01-01T01:00:00Z" {
		t.Fatalf("rfc/fallback starts: %v", starts)
	}
	if starts[4] != time.Unix(0, 0).UTC().Truncate(time.Hour).Format(time.RFC3339) {
		t.Fatalf("unix0 window: %s", starts[4])
	}

	sec, err := applyWindow(eventstreamOperator{Type: "Window", Kind: "tumbling", Duration: "1s"},
		opEvents(`{"ts":1700000000}`))
	if err != nil || len(sec) != 1 {
		t.Fatalf("unix sec: %v %+v", err, sec)
	}
}

func TestEventstreamStoredOperatorFailsOnProduce(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	path := map[string]string{"wid": ws.ID, "iid": es, "did": ds}
	for _, tc := range []struct {
		name string
		ops  []eventstreamOperator
	}{
		{"bad filter op", []eventstreamOperator{{
			Type: "Filter", Condition: &eventstreamFilterCond{Field: "n", Op: "like"},
		}}},
		{"bad window", []eventstreamOperator{{Type: "Window", Kind: "tumbling", Duration: "nope"}}},
		{"unknown apply", []eventstreamOperator{{Type: "Join"}}},
	} {
		if err := persistEventstreamOperators(st, es, tc.ops); err != nil {
			t.Fatal(err)
		}
		got := do(a.produceEventstreamEvents, admin, http.MethodPost, `{"events":[{"value":"{\"n\":1}"}]}`, path)
		if got.Code != http.StatusBadRequest || errorCode(t, got) != "EventstreamOperatorFailed" {
			t.Fatalf("%s: %d %s", tc.name, got.Code, got.Body.Bytes())
		}
	}
}

func TestEventstreamLoadPersistOperators(t *testing.T) {
	if loadEventstreamOperators(nil) != nil || loadEventstreamOperators(map[string]string{}) != nil {
		t.Fatal("empty stored ops")
	}
	if loadEventstreamOperators(map[string]string{propEventstreamOperators: "{"}) != nil {
		t.Fatal("corrupt stored ops")
	}
	got := loadEventstreamOperators(map[string]string{
		propEventstreamOperators: `[{"type":"Filter","condition":{"field":"n","op":"eq"}}]`,
	})
	if len(got) != 1 || got[0].Type != "Filter" {
		t.Fatalf("load: %+v", got)
	}
	closed, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if err := persistEventstreamOperators(closed, "missing", []eventstreamOperator{{Type: "Filter"}}); err == nil {
		t.Fatal("persist on closed store succeeded")
	}
	if err := persistEventstreamOperators(closed, "missing", []eventstreamOperator{{
		Condition: &eventstreamFilterCond{Value: make(chan int)},
	}}); err == nil {
		t.Fatal("marshal of a channel succeeded")
	}
}

func TestEventstreamOperatorHelpers(t *testing.T) {
	if _, ok := eventObject(`{"n":1}`); !ok {
		t.Fatal("object")
	}
	if obj, ok := eventObject(`not-json`); ok || obj["value"] != "not-json" {
		t.Fatalf("non-object: %v %v", obj, ok)
	}
	if obj, ok := eventObject(`null`); ok || obj["value"] != "null" {
		t.Fatalf("null: %v %v", obj, ok)
	}

	if !filterEqual(1.0, 1) || filterEqual(1.0, 2) || !filterEqual("a", "a") || filterEqual("a", "b") {
		t.Fatal("filterEqual")
	}
	if !matchFilter(map[string]any{"n": 1}, "n", "eq", 1) || matchFilter(map[string]any{"n": 1}, "n", "bogus", 1) {
		t.Fatal("matchFilter")
	}
	if filterCompare("a", "b", "xx") || filterCompare(1, 2, "xx") {
		t.Fatal("filterCompare unknown op")
	}

	for _, tc := range []struct {
		in   any
		want float64
		ok   bool
	}{
		{float64(1.5), 1.5, true},
		{float32(2), 2, true},
		{int(3), 3, true},
		{int32(4), 4, true},
		{int64(5), 5, true},
		{json.Number("6.5"), 6.5, true},
		{"7", 7, true},
		{"nope", 0, false},
		{[]int{1}, 0, false},
	} {
		got, ok := asFloat(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("asFloat(%T %v) = %v %v, want %v %v", tc.in, tc.in, got, ok, tc.want, tc.ok)
		}
	}

	if _, ok := parseEventTime("2026-01-01T00:00:00.123456789Z"); !ok {
		t.Fatal("nano")
	}
	if _, ok := parseEventTime("nope"); ok {
		t.Fatal("bad time")
	}
	if t0, ok := parseEventTime(1.7e12); !ok || t0.UnixMilli() != 1700000000000 {
		t.Fatalf("milli: %v %v", t0, ok)
	}
	if t0, ok := parseEventTime(1700000000); !ok || t0.Unix() != 1700000000 {
		t.Fatalf("sec: %v %v", t0, ok)
	}

	if kustoTableNameOK("") || kustoTableNameOK("1x") || kustoTableNameOK("a-b") || !kustoTableNameOK("_x1") {
		t.Fatal("kustoTableNameOK")
	}
	if kustoWiden("", nil) != "" || kustoWiden("", true) != "bool" ||
		kustoWiden("bool", 1.0) != "string" || kustoWiden("", "s") != "string" ||
		kustoWiden("real", int64(1)) != "real" {
		t.Fatal("kustoWiden")
	}
	if kustoCSV(nil) != "" || kustoCSV(1) != "1" || kustoCSV(`a"b,c`) != `"a""b,c"` ||
		kustoCSV("a\nb") != "\"a\nb\"" {
		t.Fatal("kustoCSV")
	}
}

func TestEventstreamBindEventhouseValidation(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	es, _ := seedEventstream(t, a, ws.ID)
	eh, db := seedEventhouse(t, a, ws.ID, "eh-bind")
	path := map[string]string{"wid": ws.ID, "iid": es}

	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"empty table", `{"type":"Eventhouse","itemId":"` + eh.ID + `","table":""}`, "InvalidRequest", http.StatusBadRequest},
		{"leading digit", `{"type":"Eventhouse","itemId":"` + eh.ID + `","table":"1clicks"}`, "InvalidRequest", http.StatusBadRequest},
		{"missing dest", `{"type":"Eventhouse","itemId":"00000000-0000-0000-0000-000000000000","table":"clicks"}`,
			"ItemNotFound", http.StatusNotFound},
	} {
		got := do(a.bindEventstreamDestination, admin, http.MethodPost, tc.body, path)
		if got.Code != tc.status || errorCode(t, got) != tc.code {
			t.Fatalf("%s: %d %s (code %s)", tc.name, got.Code, got.Body.Bytes(), errorCode(t, got))
		}
	}

	got := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Eventhouse","itemId":"`+eh.ID+`","table":"clicks","database":"`+db.DisplayName+`"}`, path)
	if got.Code != http.StatusCreated {
		t.Fatalf("bind with database: %d %s", got.Code, got.Body.Bytes())
	}
	var dest eventstreamDestination
	_ = json.Unmarshal(got.Body.Bytes(), &dest)
	if dest.Database != db.DisplayName || dest.Table != "clicks" {
		t.Fatalf("dest: %+v", dest)
	}
	again := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Eventhouse","itemId":"`+eh.ID+`","table":"clicks","database":"`+db.DisplayName+`"}`, path)
	if again.Code != http.StatusCreated {
		t.Fatalf("idempotent: %d %s", again.Code, again.Body.Bytes())
	}
	listed := do(a.listEventstreamDestinations, admin, http.MethodGet, "", path)
	var list struct{ Value []eventstreamDestination }
	_ = json.Unmarshal(listed.Body.Bytes(), &list)
	if len(list.Value) != 1 {
		t.Fatalf("idempotent list: %+v", list.Value)
	}
}

func TestEventstreamEventhouseDestNamedDatabase(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	engine := attachEngine(t, a)
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	eh, db := seedEventhouse(t, a, ws.ID, "eh-named")
	if got := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Eventhouse","itemId":"`+eh.ID+`","table":"clicks","database":"`+db.DisplayName+`"}`,
		map[string]string{"wid": ws.ID, "iid": es}); got.Code != http.StatusCreated {
		t.Fatalf("bind: %d %s", got.Code, got.Body.Bytes())
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"value":"{\"n\":1,\"flag\":true,\"msg\":\"a,b\"}"}]}`,
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	engineDB := engineDatabaseName(db.ID)
	var ingested bool
	for _, c := range engine.sent() {
		if strings.HasPrefix(c.csl, ".ingest inline into table ['clicks']") && c.db == engineDB {
			ingested = true
			if !strings.Contains(c.csl, `"a,b"`) {
				t.Fatalf("csv quoting: %s", c.csl)
			}
		}
	}
	if !ingested {
		t.Fatalf("engine calls: %+v", engine.sent())
	}
}

func TestEventstreamEventhouseDestOmitsWorkspace(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	engine := attachEngine(t, a)
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	eh, db := seedEventhouse(t, a, ws.ID, "eh-omit")
	if err := persistEventstreamDestinations(st, es, []eventstreamDestination{{
		Type: "Eventhouse", ItemID: eh.ID, Table: "clicks",
	}}); err != nil {
		t.Fatal(err)
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	engineDB := engineDatabaseName(db.ID)
	var ingested bool
	for _, c := range engine.sent() {
		if strings.HasPrefix(c.csl, ".ingest inline into table ['clicks']") && c.db == engineDB {
			ingested = true
		}
	}
	if !ingested {
		t.Fatalf("engine calls: %+v", engine.sent())
	}
}

func TestEventstreamEventhouseDestUnknownDatabase(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	attachEngine(t, a)
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	eh, _ := seedEventhouse(t, a, ws.ID, "eh-missdb")
	if err := persistEventstreamDestinations(st, es, []eventstreamDestination{{
		Type: "Eventhouse", ItemID: eh.ID, Table: "clicks", Database: "no-such-db", WorkspaceID: ws.ID,
	}}); err != nil {
		t.Fatal(err)
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusBadGateway || errorCode(t, got) != "EventstreamDestinationEventhouseFailed" {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestEventstreamEventhouseDestMissingItem(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	if err := persistEventstreamDestinations(st, es, []eventstreamDestination{{
		Type: "Eventhouse", ItemID: "00000000-0000-0000-0000-000000000000", Table: "clicks", WorkspaceID: ws.ID,
	}}); err != nil {
		t.Fatal(err)
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusBadGateway || errorCode(t, got) != "EventstreamDestinationEventhouseFailed" {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestEventstreamEventhouseDestDeleted(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	eh, _ := seedEventhouse(t, a, ws.ID, "eh-del")
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Eventhouse","itemId":"`+eh.ID+`","table":"clicks"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	if err := st.DeleteItem(ws.ID, eh.ID); err != nil {
		t.Fatal(err)
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusBadGateway || errorCode(t, got) != "EventstreamDestinationEventhouseFailed" {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestEventstreamEventhouseEmptyAfterFilterSkipsIngest(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	engine := attachEngine(t, a)
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	eh, _ := seedEventhouse(t, a, ws.ID, "eh-empty")
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Eventhouse","itemId":"`+eh.ID+`","table":"clicks"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	_ = do(a.bindEventstreamOperator, admin, http.MethodPost,
		`{"type":"Filter","condition":{"field":"n","op":"gte","value":99}}`,
		map[string]string{"wid": ws.ID, "iid": es})
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	var body struct{ Produced, Drained int }
	_ = json.Unmarshal(got.Body.Bytes(), &body)
	if body.Produced != 5 || body.Drained != 0 {
		t.Fatalf("produced/drained = %+v", body)
	}
	for _, c := range engine.sent() {
		if strings.Contains(c.csl, ".ingest inline") {
			t.Fatalf("empty batch ingested: %+v", engine.sent())
		}
	}
}

func TestEventstreamEventhouseEngineErrorOnProduce(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	engine := attachEngine(t, a)
	engine.status = http.StatusInternalServerError
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	eh, _ := seedEventhouse(t, a, ws.ID, "eh-500")
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Eventhouse","itemId":"`+eh.ID+`","table":"clicks"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusBadGateway || errorCode(t, got) != "EventstreamDestinationEventhouseFailed" {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestEventstreamEventhouseBadColumnFailsIngest(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	attachEngine(t, a)
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	eh, _ := seedEventhouse(t, a, ws.ID, "eh-col")
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Eventhouse","itemId":"`+eh.ID+`","table":"clicks"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"value":"{\"foo-bar\":1}"}]}`,
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusBadGateway || errorCode(t, got) != "EventstreamDestinationEventhouseFailed" {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	if !strings.Contains(got.Body.String(), "Kusto identifier") {
		t.Fatalf("wanted column named: %s", got.Body.Bytes())
	}
}

func TestEventstreamEventhouseAndLakehouseTogether(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	engine := attachEngine(t, a)
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	eh, db := seedEventhouse(t, a, ws.ID, "eh-both")
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"clicks"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Eventhouse","itemId":"`+eh.ID+`","table":"clicks"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	tbl, err := warehouse.ReadDeltaTable(st, lh.ID, "clicks")
	if err != nil {
		t.Fatal(err)
	}
	assertCustomEventRows(t, tbl, 5)
	engineDB := engineDatabaseName(db.ID)
	var ingested bool
	for _, c := range engine.sent() {
		if strings.HasPrefix(c.csl, ".ingest inline into table ['clicks']") && c.db == engineDB {
			ingested = true
		}
	}
	if !ingested {
		t.Fatalf("engine calls: %+v", engine.sent())
	}
}

func TestEventstreamDrainEventhouseNoopAndClosed(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{ID: "es", WorkspaceID: ws.ID, Type: "Eventstream"}
	if err := a.drainEventstreamToEventhouse(it, nil, eventBatch(`{"n":1}`)); err != nil {
		t.Fatalf("no dests: %v", err)
	}
	if err := a.drainEventstreamToEventhouse(it, map[string]string{
		propEventstreamDestinations: `[{"type":"Lakehouse","itemId":"x","table":"t"}]`,
	}, eventBatch(`{"n":1}`)); err != nil {
		t.Fatalf("non-eventhouse dest: %v", err)
	}
	eh, _ := seedEventhouse(t, a, ws.ID, "eh-closed")
	stored := map[string]string{
		propEventstreamDestinations: `[{"type":"Eventhouse","itemId":"` + eh.ID + `","table":"clicks","workspaceId":"` + ws.ID + `"}]`,
	}
	st.Close()
	if err := a.drainEventstreamToEventhouse(it, stored, eventBatch(`{"n":1}`)); err == nil {
		t.Fatal("drain on a closed store succeeded")
	}
}

func TestEventstreamKustoMgmtAndIngest(t *testing.T) {
	a, _ := newAPI(t)
	if err := a.kustoMgmt(context.Background(), "db", ".show tables"); err == nil {
		t.Fatal("no engine")
	}
	if err := a.SetKQLBackend("http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if err := a.kustoMgmt(context.Background(), "db", ".show tables"); err == nil {
		t.Fatal("refused connection")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	t.Cleanup(srv.Close)
	if err := a.SetKQLBackend(srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := a.kustoMgmt(context.Background(), "db", ".show tables"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("status: %v", err)
	}

	engine := attachEngine(t, a)
	tbl := &warehouse.Table{
		Columns: []string{"n", "flag", "msg"},
		Rows: [][]any{
			{nil, true},
			{1.0, false, `a"b`},
		},
	}
	if err := a.kustoIngestTable(context.Background(), "fabricdb", "t", tbl); err != nil {
		t.Fatal(err)
	}
	var created, ingested bool
	for _, c := range engine.sent() {
		if strings.Contains(c.csl, ".create-merge table ['t']") && strings.Contains(c.csl, "['n']:real") {
			created = true
		}
		if strings.HasPrefix(c.csl, ".ingest inline into table ['t']") {
			ingested = true
			if !strings.Contains(c.csl, `"a""b"`) {
				t.Fatalf("quoted: %s", c.csl)
			}
		}
	}
	if !created || !ingested {
		t.Fatalf("engine: %+v", engine.sent())
	}

	if err := a.kustoIngestTable(context.Background(), "fabricdb", "t", &warehouse.Table{
		Columns: []string{"bad-col"}, Rows: [][]any{{1}},
	}); err == nil || !strings.Contains(err.Error(), "Kusto identifier") {
		t.Fatalf("bad col: %v", err)
	}
	if err := a.kustoIngestTable(context.Background(), "fabricdb", "emptycol", &warehouse.Table{
		Columns: []string{"x"}, Rows: [][]any{{nil}, {}},
	}); err != nil {
		t.Fatalf("nil column: %v", err)
	}

	engine.status = http.StatusInternalServerError
	if err := a.kustoIngestTable(context.Background(), "fabricdb", "t", &warehouse.Table{
		Columns: []string{"n"}, Rows: [][]any{{1.0}},
	}); err == nil {
		t.Fatal("create-merge engine error")
	}
}

// TestEventstreamIngestQuotesColumnNames pins the emitted schema byte for byte.
//
// The drain names its columns after whatever fields the events carry, and a
// field called `kind` is ordinary in event data and refused as a bare column
// name by real Kusto — 400 / SYN0002, witnessed against kustainer in
// e2e/rti/driver.py. So the emitter quotes every name, and this test is what
// would notice if it stopped: the fake engine's schema gate refuses a bare
// keyword the way kustainer did, and the exact-string comparison catches the
// rest. `where` is in here as a name the engine happens to accept bare — the
// emitter is not supposed to be deciding which is which.
func TestEventstreamIngestQuotesColumnNames(t *testing.T) {
	a, _ := newAPI(t)
	engine := attachEngine(t, a)

	tbl := &warehouse.Table{
		Columns: []string{"kind", "where", "DeviceId", "n"},
		Rows:    [][]any{{"click", "eu", "dev-1", 1.0}},
	}
	if err := a.kustoIngestTable(context.Background(), "fabricdb", "Events", tbl); err != nil {
		t.Fatalf("keyword columns were refused by the engine: %v", err)
	}
	var create string
	for _, c := range engine.sent() {
		if strings.HasPrefix(c.csl, ".create-merge table ['Events']") {
			create = c.csl
		}
	}
	want := ".create-merge table ['Events'] (['kind']:string, ['where']:string, " +
		"['DeviceId']:string, ['n']:real)"
	if create != want {
		t.Errorf("emitted\n\t%q\nwant\n\t%q", create, want)
	}
}

// TestEventstreamIngestQuotesTableName is the same argument for the other name
// in those commands, which arrives from the destination bind rather than from
// the events: `{"table": "kind"}` passes kustoTableNameOK exactly as a `kind`
// column does.
//
// Text is the whole witness here, unlike the columns above: the fake engine's
// gate reads schema groups only, so a bare keyword TABLE name goes through it
// unremarked — as it did through kustainer, with a 400.
func TestEventstreamIngestQuotesTableName(t *testing.T) {
	a, _ := newAPI(t)
	engine := attachEngine(t, a)

	tbl := &warehouse.Table{Columns: []string{"n"}, Rows: [][]any{{1.0}}}
	if err := a.kustoIngestTable(context.Background(), "fabricdb", "kind", tbl); err != nil {
		t.Fatalf("keyword table name: %v", err)
	}
	var create, ingest bool
	for _, c := range engine.sent() {
		if c.csl == ".create-merge table ['kind'] (['n']:real)" {
			create = true
		}
		if strings.HasPrefix(c.csl, ".ingest inline into table ['kind'] <|") {
			ingest = true
		}
	}
	if !create || !ingest {
		t.Fatalf("the table name went out unquoted: %+v", engine.sent())
	}
}
