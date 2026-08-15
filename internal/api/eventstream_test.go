package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

type fakeKafka struct {
	mu     sync.Mutex
	topics map[string][][2][]byte
	fail   error
}

func (f *fakeKafka) CreateTopic(topic string) error {
	if f.fail != nil {
		return f.fail
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.topics == nil {
		f.topics = map[string][][2][]byte{}
	}
	if _, ok := f.topics[topic]; !ok {
		f.topics[topic] = nil
	}
	return nil
}

func (f *fakeKafka) Produce(topic string, key, value []byte) error {
	if f.fail != nil {
		return f.fail
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.topics == nil {
		f.topics = map[string][][2][]byte{}
	}
	f.topics[topic] = append(f.topics[topic], [2][]byte{key, value})
	return nil
}

func (f *fakeKafka) Consume(topic string, max int, wait time.Duration) ([]KafkaRecord, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	_ = wait
	f.mu.Lock()
	defer f.mu.Unlock()
	raw := f.topics[topic]
	if max <= 0 || max > len(raw) {
		max = len(raw)
	}
	out := make([]KafkaRecord, 0, max)
	for i, kv := range raw[:max] {
		out = append(out, KafkaRecord{
			Key: kv[0], Value: kv[1], Topic: topic, Partition: 0, Offset: int64(i),
			Timestamp: time.Unix(0, 0).UTC(), TimestampType: 0,
		})
	}
	return out, nil
}

func (f *fakeKafka) produced(topic string) [][2][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2][]byte(nil), f.topics[topic]...)
}

func TestEventstreamCreateMintsDatasource(t *testing.T) {
	a, st := newAPI(t)
	k := &fakeKafka{}
	a.KafkaBootstrap = "kafka:9092"
	a.Kafka = k
	ws := seedWorkspace(t, st)

	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"clicks"}`, map[string]string{"wid": ws.ID})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.Bytes())
	}
	var created struct {
		ID         string
		Properties struct {
			Streams []struct {
				ID, Name, Type string
			}
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || len(created.Properties.Streams) != 1 {
		t.Fatalf("properties: %+v", created)
	}
	ds := created.Properties.Streams[0]
	if ds.ID == "" || ds.Name != "DefaultStream" || ds.Type != "DefaultStream" {
		t.Fatalf("stream: %+v", ds)
	}
	topic := created.ID + "." + ds.ID
	k.mu.Lock()
	_, ok := k.topics[topic]
	k.mu.Unlock()
	if !ok {
		t.Fatalf("CreateTopic was not called for %s; topics=%v", topic, k.topics)
	}

	got := do(a.resolveEventstreamSource, admin, http.MethodGet, "",
		map[string]string{"iid": created.ID, "did": ds.ID})
	if got.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", got.Code, got.Body.Bytes())
	}
	var src eventstreamSource
	if err := json.Unmarshal(got.Body.Bytes(), &src); err != nil {
		t.Fatal(err)
	}
	if src.BootstrapServers != "kafka:9092" || src.Topic != topic || src.DatasourceID != ds.ID {
		t.Fatalf("source: %+v", src)
	}
}

func TestEventstreamUnknownIDsFailLoud(t *testing.T) {
	a, st := newAPI(t)
	a.KafkaBootstrap = "kafka:9092"
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"clicks"}`, map[string]string{"wid": ws.ID})
	var created struct {
		ID         string
		Properties struct {
			Streams []struct{ ID string }
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	ds := created.Properties.Streams[0].ID

	missingItem := do(a.resolveEventstreamSource, admin, http.MethodGet, "",
		map[string]string{"iid": "00000000-0000-0000-0000-000000000000", "did": ds})
	if missingItem.Code != http.StatusNotFound || errorCode(t, missingItem) != "ItemNotFound" {
		t.Fatalf("missing item: %d %s", missingItem.Code, missingItem.Body.Bytes())
	}

	wrongDS := do(a.resolveEventstreamSource, admin, http.MethodGet, "",
		map[string]string{"iid": created.ID, "did": "00000000-0000-0000-0000-000000000000"})
	if wrongDS.Code != http.StatusNotFound || errorCode(t, wrongDS) != "EventstreamSourceNotFound" {
		t.Fatalf("wrong datasource: %d %s", wrongDS.Code, wrongDS.Body.Bytes())
	}
}

func TestEventstreamResolveWithoutBrokerIs501(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"clicks"}`, map[string]string{"wid": ws.ID})
	var created struct {
		ID         string
		Properties struct {
			Streams []struct{ ID string }
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	got := do(a.resolveEventstreamSource, admin, http.MethodGet, "",
		map[string]string{"iid": created.ID, "did": created.Properties.Streams[0].ID})
	if got.Code != http.StatusNotImplemented || errorCode(t, got) != "KafkaBrokerNotAttached" {
		t.Fatalf("no broker: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestEventstreamCreateSucceedsWithoutBroker(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"mgmt-only"}`, map[string]string{"wid": ws.ID})
	if w.Code != http.StatusCreated {
		t.Fatalf("mgmt create without broker: %d %s", w.Code, w.Body.Bytes())
	}
}

func TestEventstreamProduceCustomSource(t *testing.T) {
	a, st := newAPI(t)
	k := &fakeKafka{}
	a.KafkaBootstrap = "kafka:9092"
	a.Kafka = k
	ws := seedWorkspace(t, st)
	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"clicks"}`, map[string]string{"wid": ws.ID})
	var created struct {
		ID         string
		Properties struct {
			Streams []struct{ ID string }
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	ds := created.Properties.Streams[0].ID

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"key":"k1","value":"{\"n\":1}"},{"key":"k2","value":"{\"n\":2}"}]}`,
		map[string]string{"wid": ws.ID, "iid": created.ID, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	topic := created.ID + "." + ds
	recs := k.produced(topic)
	if len(recs) != 2 || string(recs[0][0]) != "k1" || string(recs[1][1]) != `{"n":2}` {
		t.Fatalf("records: %v", recs)
	}

	denied := do(a.produceEventstreamEvents, viewer, http.MethodPost,
		`{"events":[{"value":"x"}]}`,
		map[string]string{"wid": ws.ID, "iid": created.ID, "did": ds})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("viewer produce: %d", denied.Code)
	}

	empty := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[]}`,
		map[string]string{"wid": ws.ID, "iid": created.ID, "did": ds})
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty: %d %s", empty.Code, empty.Body.Bytes())
	}
}

func TestEventstreamProduceWithoutBrokerIs501(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"clicks"}`, map[string]string{"wid": ws.ID})
	var created struct {
		ID         string
		Properties struct {
			Streams []struct{ ID string }
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"value":"x"}]}`,
		map[string]string{"wid": ws.ID, "iid": created.ID, "did": created.Properties.Streams[0].ID})
	if got.Code != http.StatusNotImplemented || errorCode(t, got) != "KafkaBrokerNotAttached" {
		t.Fatalf("no broker produce: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestSetKafkaBootstrap(t *testing.T) {
	a, _ := newAPI(t)
	if err := a.SetKafkaBootstrap("kafka:9092"); err != nil || a.KafkaBootstrap != "kafka:9092" || a.Kafka == nil {
		t.Fatalf("set: bootstrap=%q kafka=%v err=%v", a.KafkaBootstrap, a.Kafka, err)
	}
	if err := a.SetKafkaBootstrap(""); err != nil || a.Kafka != nil || a.KafkaBootstrap != "" {
		t.Fatalf("clear: bootstrap=%q kafka=%v err=%v", a.KafkaBootstrap, a.Kafka, err)
	}
	if err := a.SetKafkaBootstrap("://bad"); err == nil {
		t.Fatal("invalid bootstrap succeeded")
	}
}

func TestEventstreamConsumeReturnsKafkaRecords(t *testing.T) {
	a, st := newAPI(t)
	k := &fakeKafka{}
	a.KafkaBootstrap = "kafka:9092"
	a.Kafka = k
	ws := seedWorkspace(t, st)
	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"clicks"}`, map[string]string{"wid": ws.ID})
	var created struct {
		ID         string
		Properties struct {
			Streams []struct{ ID string }
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	ds := created.Properties.Streams[0].ID

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"key":"k1","value":"{\"n\":1}"},{"key":"k2","value":"{\"n\":2}"}]}`,
		map[string]string{"wid": ws.ID, "iid": created.ID, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}

	r := httptest.NewRequest(http.MethodGet, "/events?max=10", nil)
	r.SetPathValue("iid", created.ID)
	r.SetPathValue("did", ds)
	rec := httptest.NewRecorder()
	a.consumeEventstreamEvents(rec, r, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("consume: %d %s", rec.Code, rec.Body.Bytes())
	}
	var body struct {
		Topic   string
		Records []KafkaRecord
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Topic != created.ID+"."+ds || len(body.Records) != 2 {
		t.Fatalf("body: %+v", body)
	}
	if string(body.Records[0].Key) != "k1" || string(body.Records[1].Value) != `{"n":2}` {
		t.Fatalf("records: %+v", body.Records)
	}
	if body.Records[0].Topic != body.Topic || body.Records[1].Offset != 1 {
		t.Fatalf("kafka shape: %+v", body.Records)
	}

	capped := httptest.NewRequest(http.MethodGet, "/events?max=1", nil)
	capped.SetPathValue("iid", created.ID)
	capped.SetPathValue("did", ds)
	capRec := httptest.NewRecorder()
	a.consumeEventstreamEvents(capRec, capped, admin)
	var cappedBody struct{ Records []KafkaRecord }
	_ = json.Unmarshal(capRec.Body.Bytes(), &cappedBody)
	if capRec.Code != http.StatusOK || len(cappedBody.Records) != 1 {
		t.Fatalf("max=1: %d %s", capRec.Code, capRec.Body.Bytes())
	}
}

func TestEventstreamConsumeUnknownIDsFailLoud(t *testing.T) {
	a, st := newAPI(t)
	a.KafkaBootstrap = "kafka:9092"
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"clicks"}`, map[string]string{"wid": ws.ID})
	var created struct {
		ID         string
		Properties struct {
			Streams []struct{ ID string }
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	r := httptest.NewRequest(http.MethodGet, "/events", nil)
	r.SetPathValue("iid", "00000000-0000-0000-0000-000000000000")
	r.SetPathValue("did", created.Properties.Streams[0].ID)
	rec := httptest.NewRecorder()
	a.consumeEventstreamEvents(rec, r, admin)
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "ItemNotFound" {
		t.Fatalf("missing item: %d %s", rec.Code, rec.Body.Bytes())
	}
}

func TestEventstreamCreateSurvivesCreateTopicError(t *testing.T) {
	a, st := newAPI(t)
	a.KafkaBootstrap = "kafka:9092"
	a.Kafka = &fakeKafka{fail: errors.New("no controller")}
	ws := seedWorkspace(t, st)
	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"clicks"}`, map[string]string{"wid": ws.ID})
	if w.Code != http.StatusCreated {
		t.Fatalf("create with CreateTopic error: %d %s", w.Code, w.Body.Bytes())
	}
}

func TestEventstreamPropertiesNilWhenUnprovisioned(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, Type: "Eventstream", DisplayName: "raw"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	if got := a.eventstreamProperties(it); got != nil {
		t.Fatalf("unprovisioned properties: %v", got)
	}
}

func TestEventstreamProduceRejectsWrongItem(t *testing.T) {
	a, st := newAPI(t)
	a.KafkaBootstrap = "kafka:9092"
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	nb := do(a.typedCreate("Notebook"), admin, http.MethodPost,
		`{"displayName":"nb"}`, map[string]string{"wid": ws.ID})
	var created struct{ ID string }
	_ = json.Unmarshal(nb.Body.Bytes(), &created)
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"value":"x"}]}`,
		map[string]string{"wid": ws.ID, "iid": created.ID, "did": "ds"})
	if got.Code != http.StatusNotFound || errorCode(t, got) != "ItemNotFound" {
		t.Fatalf("notebook produce: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestEventstreamProduceWrongDatasourceIs404(t *testing.T) {
	a, st := newAPI(t)
	a.KafkaBootstrap = "kafka:9092"
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"clicks"}`, map[string]string{"wid": ws.ID})
	var created struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"value":"x"}]}`,
		map[string]string{"wid": ws.ID, "iid": created.ID, "did": "00000000-0000-0000-0000-000000000000"})
	if got.Code != http.StatusNotFound || errorCode(t, got) != "EventstreamSourceNotFound" {
		t.Fatalf("wrong ds produce: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestEventstreamProduceMalformedJSONIs400(t *testing.T) {
	a, st := newAPI(t)
	k := &fakeKafka{}
	a.KafkaBootstrap = "kafka:9092"
	a.Kafka = k
	ws := seedWorkspace(t, st)
	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"clicks"}`, map[string]string{"wid": ws.ID})
	var created struct {
		ID         string
		Properties struct {
			Streams []struct{ ID string }
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{`, map[string]string{"wid": ws.ID, "iid": created.ID, "did": created.Properties.Streams[0].ID})
	if got.Code != http.StatusBadRequest || errorCode(t, got) != "InvalidRequest" {
		t.Fatalf("malformed: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestEventstreamProduceKafkaFailureIs502(t *testing.T) {
	a, st := newAPI(t)
	k := &fakeKafka{}
	a.KafkaBootstrap = "kafka:9092"
	a.Kafka = k
	ws := seedWorkspace(t, st)
	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"clicks"}`, map[string]string{"wid": ws.ID})
	var created struct {
		ID         string
		Properties struct {
			Streams []struct{ ID string }
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	k.fail = errors.New("broker down")
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"value":"x"}]}`,
		map[string]string{"wid": ws.ID, "iid": created.ID, "did": created.Properties.Streams[0].ID})
	if got.Code != http.StatusBadGateway || errorCode(t, got) != "KafkaProduceFailed" {
		t.Fatalf("produce fail: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestEventstreamConsumeQueryAndFailures(t *testing.T) {
	a, st := newAPI(t)
	k := &fakeKafka{}
	a.KafkaBootstrap = "kafka:9092"
	a.Kafka = k
	ws := seedWorkspace(t, st)
	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"clicks"}`, map[string]string{"wid": ws.ID})
	var created struct {
		ID         string
		Properties struct {
			Streams []struct{ ID string }
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	ds := created.Properties.Streams[0].ID
	_ = do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"key":"k","value":"v"}]}`,
		map[string]string{"wid": ws.ID, "iid": created.ID, "did": ds})

	badMax := httptest.NewRequest(http.MethodGet, "/events?max=nope", nil)
	badMax.SetPathValue("iid", created.ID)
	badMax.SetPathValue("did", ds)
	rec := httptest.NewRecorder()
	a.consumeEventstreamEvents(rec, badMax, admin)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "InvalidRequest" {
		t.Fatalf("bad max: %d %s", rec.Code, rec.Body.Bytes())
	}

	zeroMax := httptest.NewRequest(http.MethodGet, "/events?max=0", nil)
	zeroMax.SetPathValue("iid", created.ID)
	zeroMax.SetPathValue("did", ds)
	rec = httptest.NewRecorder()
	a.consumeEventstreamEvents(rec, zeroMax, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("max=0: %d %s", rec.Code, rec.Body.Bytes())
	}

	capped := httptest.NewRequest(http.MethodGet, "/events?max=9999&timeoutMs=99999", nil)
	capped.SetPathValue("iid", created.ID)
	capped.SetPathValue("did", ds)
	rec = httptest.NewRecorder()
	a.consumeEventstreamEvents(rec, capped, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("capped query: %d %s", rec.Code, rec.Body.Bytes())
	}

	badWait := httptest.NewRequest(http.MethodGet, "/events?timeoutMs=nope", nil)
	badWait.SetPathValue("iid", created.ID)
	badWait.SetPathValue("did", ds)
	rec = httptest.NewRecorder()
	a.consumeEventstreamEvents(rec, badWait, admin)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "InvalidRequest" {
		t.Fatalf("bad timeout: %d %s", rec.Code, rec.Body.Bytes())
	}

	zeroWait := httptest.NewRequest(http.MethodGet, "/events?timeoutMs=0", nil)
	zeroWait.SetPathValue("iid", created.ID)
	zeroWait.SetPathValue("did", ds)
	rec = httptest.NewRecorder()
	a.consumeEventstreamEvents(rec, zeroWait, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("timeoutMs=0: %d %s", rec.Code, rec.Body.Bytes())
	}

	defaults := httptest.NewRequest(http.MethodGet, "/events", nil)
	defaults.SetPathValue("iid", created.ID)
	defaults.SetPathValue("did", ds)
	rec = httptest.NewRecorder()
	a.consumeEventstreamEvents(rec, defaults, viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer consume: %d %s", rec.Code, rec.Body.Bytes())
	}

	denied := httptest.NewRequest(http.MethodGet, "/events", nil)
	denied.SetPathValue("iid", created.ID)
	denied.SetPathValue("did", ds)
	rec = httptest.NewRecorder()
	a.consumeEventstreamEvents(rec, denied, nobody)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("nobody consume: %d", rec.Code)
	}

	wrongDS := httptest.NewRequest(http.MethodGet, "/events", nil)
	wrongDS.SetPathValue("iid", created.ID)
	wrongDS.SetPathValue("did", "00000000-0000-0000-0000-000000000000")
	rec = httptest.NewRecorder()
	a.consumeEventstreamEvents(rec, wrongDS, admin)
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "EventstreamSourceNotFound" {
		t.Fatalf("wrong ds: %d %s", rec.Code, rec.Body.Bytes())
	}

	k.fail = errors.New("read failed")
	failReq := httptest.NewRequest(http.MethodGet, "/events", nil)
	failReq.SetPathValue("iid", created.ID)
	failReq.SetPathValue("did", ds)
	rec = httptest.NewRecorder()
	a.consumeEventstreamEvents(rec, failReq, admin)
	if rec.Code != http.StatusBadGateway || errorCode(t, rec) != "KafkaConsumeFailed" {
		t.Fatalf("consume fail: %d %s", rec.Code, rec.Body.Bytes())
	}
}

func TestEventstreamConsumeWithoutBrokerIs501(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"clicks"}`, map[string]string{"wid": ws.ID})
	var created struct {
		ID         string
		Properties struct {
			Streams []struct{ ID string }
		}
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	r := httptest.NewRequest(http.MethodGet, "/events", nil)
	r.SetPathValue("iid", created.ID)
	r.SetPathValue("did", created.Properties.Streams[0].ID)
	rec := httptest.NewRecorder()
	a.consumeEventstreamEvents(rec, r, admin)
	if rec.Code != http.StatusNotImplemented || errorCode(t, rec) != "KafkaBrokerNotAttached" {
		t.Fatalf("no broker consume: %d %s", rec.Code, rec.Body.Bytes())
	}
}

func seedEventstream(t *testing.T, a *API, wsID string) (id, ds string) {
	t.Helper()
	w := do(a.typedCreate("Eventstream"), admin, http.MethodPost,
		`{"displayName":"clicks-`+store.NewID()[:8]+`"}`, map[string]string{"wid": wsID})
	if w.Code != http.StatusCreated {
		t.Fatalf("create eventstream: %d %s", w.Code, w.Body.Bytes())
	}
	var created struct {
		ID         string
		Properties struct {
			Streams []struct{ ID string }
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || len(created.Properties.Streams) == 0 || created.Properties.Streams[0].ID == "" {
		t.Fatalf("eventstream properties: %+v", created)
	}
	return created.ID, created.Properties.Streams[0].ID
}

func TestEventstreamDestinationRefusesByName(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, _ := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	path := map[string]string{"wid": ws.ID, "iid": es}

	for _, tc := range []struct {
		name, body, code string
	}{
		{"Eventhouse", `{"type":"Eventhouse","itemId":"` + lh.ID + `","table":"clicks"}`,
			"EventstreamDestinationEventhouseNotSupported"},
		{"Reflex on a Lakehouse", `{"type":"Reflex","itemId":"` + lh.ID + `"}`,
			"EventstreamDestinationNotReflex"},
		{"unknown", `{"type":"CustomEndpoint","itemId":"` + lh.ID + `","table":"clicks"}`,
			"EventstreamDestinationTypeNotSupported"},
	} {
		got := do(a.bindEventstreamDestination, admin, http.MethodPost, tc.body, path)
		if got.Code != http.StatusBadRequest || errorCode(t, got) != tc.code {
			t.Fatalf("%s: %d %s (code %s)", tc.name, got.Code, got.Body.Bytes(), errorCode(t, got))
		}
	}
}

func TestEventstreamProduceWithoutDestinationCreatesNoTable(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	if _, err := warehouse.ReadDeltaTable(st, lh.ID, "clicks"); err == nil {
		t.Fatal("produce without a destination created Tables/clicks")
	}
}

func TestEventstreamProduceWithLakehouseDestinationWritesRows(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")

	bind := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"clicks"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	if bind.Code != http.StatusCreated {
		t.Fatalf("bind: %d %s", bind.Code, bind.Body.Bytes())
	}

	listed := do(a.listEventstreamDestinations, admin, http.MethodGet, "",
		map[string]string{"wid": ws.ID, "iid": es})
	if listed.Code != http.StatusOK {
		t.Fatalf("list: %d %s", listed.Code, listed.Body.Bytes())
	}
	var list struct {
		Value []eventstreamDestination
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Value) != 1 || list.Value[0].Type != "Lakehouse" ||
		list.Value[0].ItemID != lh.ID || list.Value[0].Table != "clicks" {
		t.Fatalf("list: %+v", list.Value)
	}

	gotItem := do(a.typedGet("Eventstream"), admin, http.MethodGet, "",
		map[string]string{"wid": ws.ID, "iid": es})
	if gotItem.Code != http.StatusOK {
		t.Fatalf("get eventstream: %d %s", gotItem.Code, gotItem.Body.Bytes())
	}
	var item struct {
		Properties struct {
			Destinations []eventstreamDestination
		}
	}
	if err := json.Unmarshal(gotItem.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if len(item.Properties.Destinations) != 1 || item.Properties.Destinations[0].ItemID != lh.ID {
		t.Fatalf("GET eventstream omitted destinations: %s", gotItem.Body.Bytes())
	}

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}

	tbl, err := warehouse.ReadDeltaTable(st, lh.ID, "clicks")
	if err != nil {
		t.Fatalf("ReadDeltaTable: %v", err)
	}
	assertCustomEventRows(t, tbl, 5)
}

func TestEventstreamProduceWithLakehouseDestinationAppends(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"clicks"}`,
		map[string]string{"wid": ws.ID, "iid": es})

	first := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if first.Code != http.StatusOK {
		t.Fatalf("first produce: %d %s", first.Code, first.Body.Bytes())
	}
	second := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"value":"{\"n\":5,\"src\":\"custom\"}"},{"value":"{\"n\":6,\"src\":\"custom\"}"}]}`,
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if second.Code != http.StatusOK {
		t.Fatalf("second produce: %d %s", second.Code, second.Body.Bytes())
	}

	tbl, err := warehouse.ReadDeltaTable(st, lh.ID, "clicks")
	if err != nil {
		t.Fatalf("ReadDeltaTable: %v", err)
	}
	assertCustomEventRows(t, tbl, 7)
}

func TestEventstreamProduceWithDestinationWithoutBrokerIs501(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	bind := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"clicks"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	if bind.Code != http.StatusCreated {
		t.Fatalf("bind: %d %s", bind.Code, bind.Body.Bytes())
	}

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusNotImplemented || errorCode(t, got) != "KafkaBrokerNotAttached" {
		t.Fatalf("no broker produce: %d %s", got.Code, got.Body.Bytes())
	}
	if _, err := warehouse.ReadDeltaTable(st, lh.ID, "clicks"); err == nil {
		t.Fatal("destination wrote a table after a 501 produce")
	}
}

func TestEventstreamBindDestinationValidation(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	es, _ := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
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
		{"empty type", `{"type":"","itemId":"` + lh.ID + `","table":"clicks"}`, "InvalidRequest", http.StatusBadRequest},
		{"missing itemId", `{"type":"Lakehouse","table":"clicks"}`, "InvalidRequest", http.StatusBadRequest},
		{"empty table", `{"type":"Lakehouse","itemId":"` + lh.ID + `","table":""}`, "InvalidRequest", http.StatusBadRequest},
		{"slash table", `{"type":"Lakehouse","itemId":"` + lh.ID + `","table":"Tables/clicks"}`, "InvalidRequest", http.StatusBadRequest},
		{"missing dest", `{"type":"Lakehouse","itemId":"00000000-0000-0000-0000-000000000000","table":"clicks"}`,
			"ItemNotFound", http.StatusNotFound},
		{"not a lakehouse", `{"type":"Lakehouse","itemId":"` + notebook.ID + `","table":"clicks"}`,
			"EventstreamDestinationNotLakehouse", http.StatusBadRequest},
	} {
		got := do(a.bindEventstreamDestination, admin, http.MethodPost, tc.body, path)
		if got.Code != tc.status || errorCode(t, got) != tc.code {
			t.Fatalf("%s: %d %s (code %s)", tc.name, got.Code, got.Body.Bytes(), errorCode(t, got))
		}
	}

	wrongItem := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"clicks"}`,
		map[string]string{"wid": ws.ID, "iid": notebook.ID})
	if wrongItem.Code != http.StatusNotFound || errorCode(t, wrongItem) != "ItemNotFound" {
		t.Fatalf("notebook bind: %d %s", wrongItem.Code, wrongItem.Body.Bytes())
	}

	trimmed := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"  clicks  ","workspaceId":"`+ws.ID+`"}`,
		path)
	if trimmed.Code != http.StatusCreated {
		t.Fatalf("trim table: %d %s", trimmed.Code, trimmed.Body.Bytes())
	}
	var dest eventstreamDestination
	if err := json.Unmarshal(trimmed.Body.Bytes(), &dest); err != nil {
		t.Fatal(err)
	}
	if dest.Table != "clicks" || dest.WorkspaceID != ws.ID {
		t.Fatalf("trimmed dest: %+v", dest)
	}

	again := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"clicks"}`, path)
	if again.Code != http.StatusCreated {
		t.Fatalf("idempotent bind: %d %s", again.Code, again.Body.Bytes())
	}
	listed := do(a.listEventstreamDestinations, admin, http.MethodGet, "", path)
	var list struct{ Value []eventstreamDestination }
	_ = json.Unmarshal(listed.Body.Bytes(), &list)
	if listed.Code != http.StatusOK || len(list.Value) != 1 {
		t.Fatalf("idempotent list: %d %+v", listed.Code, list.Value)
	}
}

func TestEventstreamDestinationRBAC(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	es, _ := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	path := map[string]string{"wid": ws.ID, "iid": es}
	body := `{"type":"Lakehouse","itemId":"` + lh.ID + `","table":"clicks"}`

	if got := do(a.bindEventstreamDestination, viewer, http.MethodPost, body, path); got.Code != http.StatusForbidden {
		t.Fatalf("viewer bind: %d %s", got.Code, got.Body.Bytes())
	}
	if got := do(a.bindEventstreamDestination, nobody, http.MethodPost, body, path); got.Code != http.StatusForbidden {
		t.Fatalf("nobody bind: %d %s", got.Code, got.Body.Bytes())
	}

	empty := do(a.listEventstreamDestinations, viewer, http.MethodGet, "", path)
	if empty.Code != http.StatusOK {
		t.Fatalf("viewer list: %d %s", empty.Code, empty.Body.Bytes())
	}
	var list struct{ Value []eventstreamDestination }
	if err := json.Unmarshal(empty.Body.Bytes(), &list); err != nil || list.Value == nil || len(list.Value) != 0 {
		t.Fatalf("empty list: %s", empty.Body.Bytes())
	}
	if got := do(a.listEventstreamDestinations, nobody, http.MethodGet, "", path); got.Code != http.StatusForbidden {
		t.Fatalf("nobody list: %d", got.Code)
	}
}

func TestEventstreamBindCrossWorkspaceDestination(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	destWS := seedWorkspaceNamed(t, st, "dest-ws")
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, destWS.ID, "lh")

	denied := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"clicks","workspaceId":"00000000-0000-0000-0000-000000000000"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	if denied.Code != http.StatusNotFound || errorCode(t, denied) != "WorkspaceNotFound" {
		t.Fatalf("missing dest workspace: %d %s", denied.Code, denied.Body.Bytes())
	}

	foreign := &store.Workspace{DisplayName: "foreign"}
	if err := st.CreateWorkspace(foreign, store.Principal{ID: "other-owner", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	foreignLH := seedLakehouse(t, st, foreign.ID, "foreign-lh")
	forbidden := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+foreignLH.ID+`","table":"clicks","workspaceId":"`+foreign.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("foreign dest workspace: %d %s", forbidden.Code, forbidden.Body.Bytes())
	}

	bind := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"clicks","workspaceId":"`+destWS.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	if bind.Code != http.StatusCreated {
		t.Fatalf("cross-workspace bind: %d %s", bind.Code, bind.Body.Bytes())
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	tbl, err := warehouse.ReadDeltaTable(st, lh.ID, "clicks")
	if err != nil {
		t.Fatalf("ReadDeltaTable: %v", err)
	}
	assertCustomEventRows(t, tbl, 5)
}

func TestEventstreamProduceNonJSONValueColumn(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"plain"}`,
		map[string]string{"wid": ws.ID, "iid": es})

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"value":"not-json"},{"value":"null"},{"value":"[1,2]"},{"value":"{\"n\":1}"}]}`,
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	tbl, err := warehouse.ReadDeltaTable(st, lh.ID, "plain")
	if err != nil {
		t.Fatalf("ReadDeltaTable: %v", err)
	}
	if len(tbl.Columns) != 1 || tbl.Columns[0] != "value" || len(tbl.Rows) != 4 {
		t.Fatalf("value column: %+v", tbl)
	}
	want := []string{"not-json", "null", "[1,2]", `{"n":1}`}
	for i, row := range tbl.Rows {
		got, _ := row[0].(string)
		if got != want[i] {
			t.Fatalf("row %d: %q want %q", i, got, want[i])
		}
	}
}

func TestEventstreamProduceNestedJSONCells(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"nested"}`,
		map[string]string{"wid": ws.ID, "iid": es})

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"value":"{\"ok\":true,\"meta\":{\"k\":1},\"tags\":[\"a\"]}"}]}`,
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	tbl, err := warehouse.ReadDeltaTable(st, lh.ID, "nested")
	if err != nil {
		t.Fatalf("ReadDeltaTable: %v", err)
	}
	okCol, metaCol, tagsCol := colIndex(tbl.Columns, "ok"), colIndex(tbl.Columns, "meta"), colIndex(tbl.Columns, "tags")
	if okCol < 0 || metaCol < 0 || tagsCol < 0 || len(tbl.Rows) != 1 {
		t.Fatalf("columns: %v rows: %v", tbl.Columns, tbl.Rows)
	}
	if ok, _ := tbl.Rows[0][okCol].(bool); !ok {
		t.Fatalf("ok: %v", tbl.Rows[0][okCol])
	}
	meta, _ := tbl.Rows[0][metaCol].(string)
	if !strings.Contains(meta, `"k"`) {
		t.Fatalf("meta cell: %q", meta)
	}
	tags, _ := tbl.Rows[0][tagsCol].(string)
	if !strings.Contains(tags, `"a"`) {
		t.Fatalf("tags cell: %q", tags)
	}
}

func TestEventstreamProduceUnionObjectKeys(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"union"}`,
		map[string]string{"wid": ws.ID, "iid": es})

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"value":"{\"a\":1}"},{"value":"{\"b\":2}"}]}`,
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	tbl, err := warehouse.ReadDeltaTable(st, lh.ID, "union")
	if err != nil || len(tbl.Rows) != 2 {
		t.Fatalf("ReadDeltaTable: %v %+v", err, tbl)
	}
	aCol, bCol := colIndex(tbl.Columns, "a"), colIndex(tbl.Columns, "b")
	if aCol < 0 || bCol < 0 {
		t.Fatalf("columns: %v", tbl.Columns)
	}
	if tbl.Rows[0][aCol] == nil || tbl.Rows[1][bCol] == nil {
		t.Fatalf("union cells: %v", tbl.Rows)
	}
}

func TestEventstreamMultipleLakehouseDestinations(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	one := seedLakehouse(t, st, ws.ID, "one")
	two := seedLakehouse(t, st, ws.ID, "two")
	for _, lh := range []*store.Item{one, two} {
		got := do(a.bindEventstreamDestination, admin, http.MethodPost,
			`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"clicks"}`,
			map[string]string{"wid": ws.ID, "iid": es})
		if got.Code != http.StatusCreated {
			t.Fatalf("bind %s: %d %s", lh.DisplayName, got.Code, got.Body.Bytes())
		}
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	for _, lh := range []*store.Item{one, two} {
		tbl, err := warehouse.ReadDeltaTable(st, lh.ID, "clicks")
		if err != nil {
			t.Fatalf("%s: %v", lh.DisplayName, err)
		}
		assertCustomEventRows(t, tbl, 5)
	}
}

func TestEventstreamSkipsNonLakehouseStoredDestination(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	if err := persistEventstreamDestinations(st, es, []eventstreamDestination{{
		Type: "Eventhouse", ItemID: lh.ID, Table: "clicks", WorkspaceID: ws.ID,
	}}); err != nil {
		t.Fatal(err)
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	if _, err := warehouse.ReadDeltaTable(st, lh.ID, "clicks"); err == nil {
		t.Fatal("Eventhouse dest in storage was drained")
	}
}

func TestEventstreamDrainUsesEventstreamWorkspaceWhenDestOmitsIt(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	if err := persistEventstreamDestinations(st, es, []eventstreamDestination{{
		Type: "Lakehouse", ItemID: lh.ID, Table: "clicks",
	}}); err != nil {
		t.Fatal(err)
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	tbl, err := warehouse.ReadDeltaTable(st, lh.ID, "clicks")
	if err != nil {
		t.Fatalf("ReadDeltaTable: %v", err)
	}
	assertCustomEventRows(t, tbl, 5)
}

func TestEventstreamCorruptDestinationsAreIgnored(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	if err := st.SetItemProperties(es, map[string]string{propEventstreamDestinations: "not-json"}); err != nil {
		t.Fatal(err)
	}
	listed := do(a.listEventstreamDestinations, admin, http.MethodGet, "",
		map[string]string{"wid": ws.ID, "iid": es})
	var list struct{ Value []eventstreamDestination }
	_ = json.Unmarshal(listed.Body.Bytes(), &list)
	if listed.Code != http.StatusOK || len(list.Value) != 0 {
		t.Fatalf("corrupt list: %d %s", listed.Code, listed.Body.Bytes())
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	if _, err := warehouse.ReadDeltaTable(st, lh.ID, "clicks"); err == nil {
		t.Fatal("corrupt destinations created a table")
	}
}

func TestEventstreamDestinationAttributesWrites(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"clicks"}`,
		map[string]string{"wid": ws.ID, "iid": es})

	sub := st.Subscribe()
	defer sub.Close()
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}

	var files, tables int
	for _, ev := range drainEvents(t, sub.C) {
		if ev.Attribution == nil {
			continue
		}
		if ev.Attribution.JobID != es || ev.Attribution.ActivityName != "Eventstream" {
			t.Fatalf("attribution: %+v on %+v", ev.Attribution, ev)
		}
		switch ev.Kind {
		case store.KindFile:
			files++
		case store.KindTable:
			tables++
		}
	}
	if files < 2 || tables < 1 {
		t.Fatalf("attributed file=%d table=%d", files, tables)
	}
}

func TestEventstreamDrainWriteFailure(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	es, _ := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	it := &store.Item{ID: es, WorkspaceID: ws.ID, Type: "Eventstream", DisplayName: "clicks"}
	stored := map[string]string{
		propEventstreamDestinations: `[{"type":"Lakehouse","itemId":"` + lh.ID + `","table":"clicks","workspaceId":"` + ws.ID + `"}]`,
	}
	st.Close()
	if err := a.drainEventstreamToLakehouse(it, stored, eventBatch(`{"n":1}`)); err == nil {
		t.Fatal("drain on a closed store succeeded")
	}
}

func seedReflexDest(t *testing.T, a *API, st *store.Store, wsID, eventstreamID, pipeDef string) (reflex, pipe *store.Item) {
	t.Helper()
	reflex = &store.Item{WorkspaceID: wsID, Type: "Reflex", DisplayName: "stream-watcher-" + store.NewID()[:8]}
	if err := st.CreateItem(reflex, nil); err != nil {
		t.Fatal(err)
	}
	pipe = createPipeline(t, st, wsID, pipeDef)
	trig := do(a.createEventTrigger, admin, http.MethodPost,
		`{"displayName":"on-clicks","eventType":"`+store.EventEventstreamReceived+`",`+
			`"source":{"itemId":"`+eventstreamID+`"},`+
			`"action":{"itemId":"`+pipe.ID+`","jobType":"Pipeline"}}`,
		map[string]string{"wid": wsID, "iid": reflex.ID})
	if trig.Code != http.StatusCreated {
		t.Fatalf("create stream trigger: %d %s", trig.Code, trig.Body.Bytes())
	}
	return reflex, pipe
}

func TestEventstreamBindReflexDestination(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	es, _ := seedEventstream(t, a, ws.ID)
	reflex, _ := seedReflexDest(t, a, st, ws.ID, es, waitDef)

	bind := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	if bind.Code != http.StatusCreated {
		t.Fatalf("bind: %d %s", bind.Code, bind.Body.Bytes())
	}
	var dest eventstreamDestination
	if err := json.Unmarshal(bind.Body.Bytes(), &dest); err != nil {
		t.Fatal(err)
	}
	if dest.Type != "Reflex" || dest.ItemID != reflex.ID || dest.Table != "" {
		t.Fatalf("dest: %+v", dest)
	}

	again := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	if again.Code != http.StatusCreated {
		t.Fatalf("idempotent: %d %s", again.Code, again.Body.Bytes())
	}
	listed := do(a.listEventstreamDestinations, admin, http.MethodGet, "",
		map[string]string{"wid": ws.ID, "iid": es})
	var list struct{ Value []eventstreamDestination }
	_ = json.Unmarshal(listed.Body.Bytes(), &list)
	if listed.Code != http.StatusOK || len(list.Value) != 1 {
		t.Fatalf("list: %d %+v", listed.Code, list.Value)
	}
}

func TestEventstreamProduceWithReflexDestinationStartsJobs(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	reflex, pipe := seedReflexDest(t, a, st, ws.ID, es, waitDef)
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	jobs, err := st.ListItemJobInstances(pipe.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 5 {
		t.Fatalf("jobs = %d, want 5", len(jobs))
	}
	for _, j := range jobs {
		if j.InvokeType != store.InvokeEventTriggered {
			t.Fatalf("invokeType = %q", j.InvokeType)
		}
	}
}

func TestEventstreamProduceReflexTriggerEventValue(t *testing.T) {
	def := `{"properties":{"activities":[
		{"name":"Capture","type":"SetVariable","typeProperties":{
			"variableName":"seen","value":"@concat(pipeline()?.TriggerEvent?.Key,'|',pipeline()?.TriggerEvent?.EventType,'|',pipeline()?.TriggerEvent?.Value)"}}],
		"variables":{"seen":{"type":"String"}}}}`
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	reflex, pipe := seedReflexDest(t, a, st, ws.ID, es, def)
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"key":"k0","value":"{\"n\":0,\"src\":\"custom\"}"}]}`,
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	jobs, err := st.ListItemJobInstances(pipe.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %v %d", err, len(jobs))
	}
	status, detail := awaitPipelineRun(t, st, jobs[0].ID)
	if status != "Succeeded" {
		t.Fatalf("status = %s: %s", status, detail)
	}
	if !strings.Contains(detail, `k0`) ||
		!strings.Contains(detail, store.EventEventstreamReceived) ||
		!strings.Contains(detail, `n`) || !strings.Contains(detail, `custom`) {
		t.Fatalf("TriggerEvent Key/EventType/Value did not reach the pipeline: %s", detail)
	}
}

func TestEventstreamProduceWithoutReflexDestDoesNotFire(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	_, pipe := seedReflexDest(t, a, st, ws.ID, es, waitDef)

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	jobs, err := st.ListItemJobInstances(pipe.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("trigger without a dest started %d jobs", len(jobs))
	}
}

func TestEventstreamProduceReflexDoesNotFireFileTrigger(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	reflex := &store.Item{WorkspaceID: ws.ID, Type: "Reflex", DisplayName: "file-watcher"}
	if err := st.CreateItem(reflex, nil); err != nil {
		t.Fatal(err)
	}
	pipe := createPipeline(t, st, ws.ID, waitDef)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	trig := do(a.createEventTrigger, admin, http.MethodPost,
		`{"displayName":"on-file","eventType":"`+store.EventFileCreated+`",`+
			`"source":{"itemId":"`+lh.ID+`"},`+
			`"action":{"itemId":"`+pipe.ID+`","jobType":"Pipeline"}}`,
		map[string]string{"wid": ws.ID, "iid": reflex.ID})
	if trig.Code != http.StatusCreated {
		t.Fatalf("file trigger: %d %s", trig.Code, trig.Body.Bytes())
	}
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	jobs, err := st.ListItemJobInstances(pipe.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("a FileCreated trigger fired on stream produce: %d", len(jobs))
	}
}

func TestEventstreamProduceReflexWithoutBrokerIs501(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	reflex, pipe := seedReflexDest(t, a, st, ws.ID, es, waitDef)
	bind := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	if bind.Code != http.StatusCreated {
		t.Fatalf("bind: %d %s", bind.Code, bind.Body.Bytes())
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusNotImplemented || errorCode(t, got) != "KafkaBrokerNotAttached" {
		t.Fatalf("no broker: %d %s", got.Code, got.Body.Bytes())
	}
	jobs, err := st.ListItemJobInstances(pipe.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("501 produce started %d jobs", len(jobs))
	}
}

func TestEventstreamBindReflexDestinationValidation(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	es, _ := seedEventstream(t, a, ws.ID)
	reflex, _ := seedReflexDest(t, a, st, ws.ID, es, waitDef)
	path := map[string]string{"wid": ws.ID, "iid": es}

	missing := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex"}`, path)
	if missing.Code != http.StatusBadRequest || errorCode(t, missing) != "InvalidRequest" {
		t.Fatalf("missing itemId: %d %s", missing.Code, missing.Body.Bytes())
	}
	unknown := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"00000000-0000-0000-0000-000000000000"}`, path)
	if unknown.Code != http.StatusNotFound || errorCode(t, unknown) != "ItemNotFound" {
		t.Fatalf("missing dest: %d %s", unknown.Code, unknown.Body.Bytes())
	}
	lakeOnReflex := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+reflex.ID+`","table":"clicks"}`, path)
	if lakeOnReflex.Code != http.StatusBadRequest || errorCode(t, lakeOnReflex) != "EventstreamDestinationNotLakehouse" {
		t.Fatalf("Lakehouse on Reflex: %d %s", lakeOnReflex.Code, lakeOnReflex.Body.Bytes())
	}

	cleared := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`","table":"clicks"}`, path)
	if cleared.Code != http.StatusCreated {
		t.Fatalf("table on Reflex: %d %s", cleared.Code, cleared.Body.Bytes())
	}
	var dest eventstreamDestination
	if err := json.Unmarshal(cleared.Body.Bytes(), &dest); err != nil {
		t.Fatal(err)
	}
	if dest.Table != "" || dest.Type != "Reflex" || dest.WorkspaceID != ws.ID {
		t.Fatalf("Reflex dest kept a table: %+v", dest)
	}

	gotItem := do(a.typedGet("Eventstream"), admin, http.MethodGet, "", path)
	if gotItem.Code != http.StatusOK {
		t.Fatalf("get eventstream: %d %s", gotItem.Code, gotItem.Body.Bytes())
	}
	var item struct {
		Properties struct {
			Destinations []eventstreamDestination
		}
	}
	if err := json.Unmarshal(gotItem.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if len(item.Properties.Destinations) != 1 || item.Properties.Destinations[0].Type != "Reflex" ||
		item.Properties.Destinations[0].ItemID != reflex.ID || item.Properties.Destinations[0].Table != "" {
		t.Fatalf("GET eventstream omitted Reflex dest: %s", gotItem.Body.Bytes())
	}

	if got := do(a.bindEventstreamDestination, viewer, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`"}`, path); got.Code != http.StatusForbidden {
		t.Fatalf("viewer bind: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestEventstreamProduceReflexDisabledTriggerDoesNotFire(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	reflex, pipe := seedReflexDest(t, a, st, ws.ID, es, waitDef)
	trigs, err := st.ListEventTriggers(reflex.ID)
	if err != nil || len(trigs) != 1 {
		t.Fatalf("triggers: %v %d", err, len(trigs))
	}
	tid := map[string]string{"wid": ws.ID, "iid": reflex.ID, "tid": trigs[0].ID}
	if w := do(a.updateEventTrigger, admin, http.MethodPatch, `{"enabled":false}`, tid); w.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", w.Code, w.Body.Bytes())
	}
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	jobs, err := st.ListItemJobInstances(pipe.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("disabled trigger started %d jobs", len(jobs))
	}
}

func TestEventstreamProduceReflexWrongSourceDoesNotFire(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	other, _ := seedEventstream(t, a, ws.ID)
	reflex, pipe := seedReflexDest(t, a, st, ws.ID, other, waitDef)
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	jobs, err := st.ListItemJobInstances(pipe.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("wrong-source trigger started %d jobs", len(jobs))
	}
}

func TestEventstreamProduceReflexAndLakehouseTogether(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	reflex, pipe := seedReflexDest(t, a, st, ws.ID, es, waitDef)
	path := map[string]string{"wid": ws.ID, "iid": es}
	if got := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Lakehouse","itemId":"`+lh.ID+`","table":"clicks"}`, path); got.Code != http.StatusCreated {
		t.Fatalf("lake bind: %d %s", got.Code, got.Body.Bytes())
	}
	if got := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`"}`, path); got.Code != http.StatusCreated {
		t.Fatalf("reflex bind: %d %s", got.Code, got.Body.Bytes())
	}

	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	tbl, err := warehouse.ReadDeltaTable(st, lh.ID, "clicks")
	if err != nil {
		t.Fatalf("ReadDeltaTable: %v", err)
	}
	assertCustomEventRows(t, tbl, 5)
	jobs, err := st.ListItemJobInstances(pipe.ID)
	if err != nil || len(jobs) != 5 {
		t.Fatalf("jobs: %v %d", err, len(jobs))
	}
}

func TestEventstreamMultipleReflexDestinations(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	one, pipeOne := seedReflexDest(t, a, st, ws.ID, es, waitDef)
	two, pipeTwo := seedReflexDest(t, a, st, ws.ID, es, waitDef)
	for _, reflex := range []*store.Item{one, two} {
		got := do(a.bindEventstreamDestination, admin, http.MethodPost,
			`{"type":"Reflex","itemId":"`+reflex.ID+`"}`,
			map[string]string{"wid": ws.ID, "iid": es})
		if got.Code != http.StatusCreated {
			t.Fatalf("bind %s: %d %s", reflex.ID, got.Code, got.Body.Bytes())
		}
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	for _, pipe := range []*store.Item{pipeOne, pipeTwo} {
		jobs, err := st.ListItemJobInstances(pipe.ID)
		if err != nil || len(jobs) != 5 {
			t.Fatalf("%s jobs: %v %d", pipe.ID, err, len(jobs))
		}
	}
}

func TestEventstreamBindCrossWorkspaceReflexDestination(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	destWS := seedWorkspaceNamed(t, st, "dest-ws")
	es, ds := seedEventstream(t, a, ws.ID)
	reflex, pipe := seedReflexDest(t, a, st, destWS.ID, es, waitDef)

	foreign := &store.Workspace{DisplayName: "foreign"}
	if err := st.CreateWorkspace(foreign, store.Principal{ID: "other-owner", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	foreignReflex := &store.Item{WorkspaceID: foreign.ID, Type: "Reflex", DisplayName: "foreign-watcher"}
	if err := st.CreateItem(foreignReflex, nil); err != nil {
		t.Fatal(err)
	}
	forbidden := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+foreignReflex.ID+`","workspaceId":"`+foreign.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("foreign dest workspace: %d %s", forbidden.Code, forbidden.Body.Bytes())
	}

	bind := do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`","workspaceId":"`+destWS.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	if bind.Code != http.StatusCreated {
		t.Fatalf("cross-workspace bind: %d %s", bind.Code, bind.Body.Bytes())
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	jobs, err := st.ListItemJobInstances(pipe.ID)
	if err != nil || len(jobs) != 5 {
		t.Fatalf("jobs: %v %d", err, len(jobs))
	}
}

func TestEventstreamProduceReflexSecondProduceFiresAgain(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	reflex, pipe := seedReflexDest(t, a, st, ws.ID, es, waitDef)
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})

	first := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if first.Code != http.StatusOK {
		t.Fatalf("first produce: %d %s", first.Code, first.Body.Bytes())
	}
	second := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"value":"{\"n\":5}"},{"value":"{\"n\":6}"}]}`,
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if second.Code != http.StatusOK {
		t.Fatalf("second produce: %d %s", second.Code, second.Body.Bytes())
	}
	jobs, err := st.ListItemJobInstances(pipe.ID)
	if err != nil || len(jobs) != 7 {
		t.Fatalf("jobs: %v %d", err, len(jobs))
	}
}

func TestEventstreamProduceReflexMissingDestItemIs502(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	if err := persistEventstreamDestinations(st, es, []eventstreamDestination{{
		Type: "Reflex", ItemID: "00000000-0000-0000-0000-000000000000", WorkspaceID: ws.ID,
	}}); err != nil {
		t.Fatal(err)
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusBadGateway || errorCode(t, got) != "EventstreamDestinationReflexFailed" {
		t.Fatalf("missing dest item: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestEventstreamProduceReflexWrongStoredTypeIs502(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	lh := seedLakehouse(t, st, ws.ID, "lh")
	if err := persistEventstreamDestinations(st, es, []eventstreamDestination{{
		Type: "Reflex", ItemID: lh.ID, WorkspaceID: ws.ID,
	}}); err != nil {
		t.Fatal(err)
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusBadGateway || errorCode(t, got) != "EventstreamDestinationReflexFailed" {
		t.Fatalf("stored Lakehouse as Reflex: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestEventstreamProduceReflexDeletedTargetIs502(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	reflex, pipe := seedReflexDest(t, a, st, ws.ID, es, waitDef)
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	if err := st.DeleteItem(ws.ID, pipe.ID); err != nil {
		t.Fatal(err)
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		`{"events":[{"value":"{\"n\":0}"}]}`,
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusBadGateway || errorCode(t, got) != "EventstreamDestinationReflexFailed" {
		t.Fatalf("deleted target: %d %s", got.Code, got.Body.Bytes())
	}
}

func TestEventstreamDrainReflexUsesEventstreamWorkspaceWhenDestOmitsIt(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	reflex, pipe := seedReflexDest(t, a, st, ws.ID, es, waitDef)
	if err := persistEventstreamDestinations(st, es, []eventstreamDestination{{
		Type: "Reflex", ItemID: reflex.ID,
	}}); err != nil {
		t.Fatal(err)
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("produce: %d %s", got.Code, got.Body.Bytes())
	}
	jobs, err := st.ListItemJobInstances(pipe.ID)
	if err != nil || len(jobs) != 5 {
		t.Fatalf("jobs: %v %d", err, len(jobs))
	}
}

func TestEventstreamDrainReflexClosedStore(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	es, _ := seedEventstream(t, a, ws.ID)
	reflex, _ := seedReflexDest(t, a, st, ws.ID, es, waitDef)
	it := &store.Item{ID: es, WorkspaceID: ws.ID, Type: "Eventstream", DisplayName: "clicks"}
	stored := map[string]string{
		propEventstreamDestinations: `[{"type":"Reflex","itemId":"` + reflex.ID + `","workspaceId":"` + ws.ID + `"}]`,
	}
	st.Close()
	if err := a.drainEventstreamToReflex(it, stored, eventBatch(`{"n":1}`)); err == nil {
		t.Fatal("drain on a closed store succeeded")
	}
}

func TestEventstreamProduceReflexActivationCapSkips(t *testing.T) {
	a, st := newAPI(t)
	a.Kafka = &fakeKafka{}
	ws := seedWorkspace(t, st)
	es, ds := seedEventstream(t, a, ws.ID)
	reflex, pipe := seedReflexDest(t, a, st, ws.ID, es, waitDef)
	_ = do(a.bindEventstreamDestination, admin, http.MethodPost,
		`{"type":"Reflex","itemId":"`+reflex.ID+`"}`,
		map[string]string{"wid": ws.ID, "iid": es})
	trigs, err := st.ListEventTriggers(reflex.ID)
	if err != nil || len(trigs) != 1 {
		t.Fatalf("triggers: %v %d", err, len(trigs))
	}
	for i := 0; i < maxTriggerActivations; i++ {
		if !a.firing.enter(trigs[0].ID) {
			t.Fatalf("could not fill activation cap at %d", i)
		}
	}
	got := do(a.produceEventstreamEvents, admin, http.MethodPost,
		fiveCustomEventsJSON(),
		map[string]string{"wid": ws.ID, "iid": es, "did": ds})
	if got.Code != http.StatusOK {
		t.Fatalf("capped produce: %d %s", got.Code, got.Body.Bytes())
	}
	jobs, err := st.ListItemJobInstances(pipe.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("capped trigger started %d jobs", len(jobs))
	}
}

func TestStreamTriggerEventParamsShape(t *testing.T) {
	es := &store.Item{ID: "es", WorkspaceID: "w", Type: "Eventstream"}
	got := streamTriggerEventParams(es, "k0", `{"n":1}`)
	for k, want := range map[string]string{
		"EventType": store.EventEventstreamReceived, "Source": "es", "Subject": "es",
		"Key": "k0", "Value": `{"n":1}`, "WorkspaceId": "w", "ItemId": "es",
	} {
		if got[k] != want {
			t.Fatalf("%s = %v, want %q", k, got[k], want)
		}
	}
	if _, ok := got["FileName"]; ok {
		t.Fatal("stream TriggerEvent carried FileName")
	}
	if _, ok := got["FolderPath"]; ok {
		t.Fatal("stream TriggerEvent carried FolderPath")
	}
}

func TestEventstreamDrainReflexNoop(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{ID: "es", WorkspaceID: ws.ID, Type: "Eventstream"}
	if err := a.drainEventstreamToReflex(it, nil, eventBatch(`{"n":1}`)); err != nil {
		t.Fatalf("no dests: %v", err)
	}
	if err := a.drainEventstreamToReflex(it, map[string]string{
		propEventstreamDestinations: `[{"type":"Lakehouse","itemId":"x","table":"t"}]`,
	}, eventBatch(`{"n":1}`)); err != nil {
		t.Fatalf("non-reflex dest: %v", err)
	}
}

func TestEventsToDeltaTableAndLoadDestinations(t *testing.T) {
	if eventsToDeltaTable(eventstreamEventBatch{}) != nil {
		t.Fatal("empty batch")
	}
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, Type: "Eventstream", DisplayName: "e"}
	if err := a.drainEventstreamToLakehouse(it, nil, eventBatch(`{"n":1}`)); err != nil {
		t.Fatalf("no dests: %v", err)
	}
	if err := a.drainEventstreamToLakehouse(it, map[string]string{
		propEventstreamDestinations: `[{"type":"Lakehouse","itemId":"x","table":"t"}]`,
	}, eventstreamEventBatch{}); err != nil {
		t.Fatalf("empty events: %v", err)
	}
	if loadEventstreamDestinations(nil) != nil || loadEventstreamDestinations(map[string]string{}) != nil {
		t.Fatal("empty stored dests")
	}
	if loadEventstreamDestinations(map[string]string{propEventstreamDestinations: "{"}) != nil {
		t.Fatal("corrupt stored dests")
	}
	got := loadEventstreamDestinations(map[string]string{
		propEventstreamDestinations: `[{"type":"Lakehouse","itemId":"i","table":"t"}]`,
	})
	if len(got) != 1 || got[0].ItemID != "i" || got[0].Table != "t" {
		t.Fatalf("load: %+v", got)
	}

	closed, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if err := persistEventstreamDestinations(closed, "missing", []eventstreamDestination{{Type: "Lakehouse"}}); err == nil {
		t.Fatal("persist on closed store succeeded")
	}
}

func eventBatch(values ...string) eventstreamEventBatch {
	var body eventstreamEventBatch
	for _, v := range values {
		body.Events = append(body.Events, struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}{Value: v})
	}
	return body
}

func fiveCustomEventsJSON() string {
	return `{"events":[` +
		`{"value":"{\"n\":0,\"src\":\"custom\"}"},` +
		`{"value":"{\"n\":1,\"src\":\"custom\"}"},` +
		`{"value":"{\"n\":2,\"src\":\"custom\"}"},` +
		`{"value":"{\"n\":3,\"src\":\"custom\"}"},` +
		`{"value":"{\"n\":4,\"src\":\"custom\"}"}` +
		`]}`
}

func assertCustomEventRows(t *testing.T, tbl *warehouse.Table, want int) {
	t.Helper()
	if tbl == nil || len(tbl.Rows) != want {
		t.Fatalf("rows: want %d, got %v", want, tbl)
	}
	nCol, srcCol := colIndex(tbl.Columns, "n"), colIndex(tbl.Columns, "src")
	if nCol < 0 || srcCol < 0 {
		t.Fatalf("columns %v missing n/src", tbl.Columns)
	}
	seen := map[int]bool{}
	for _, row := range tbl.Rows {
		n := intFromCell(t, row[nCol])
		src, _ := row[srcCol].(string)
		if src != "custom" {
			t.Fatalf("src=%v row=%v", row[srcCol], row)
		}
		if n < 0 || n >= want || seen[n] {
			t.Fatalf("unexpected n=%d in %v", n, tbl.Rows)
		}
		seen[n] = true
	}
	if len(seen) != want {
		t.Fatalf("n values: %v", seen)
	}
}

func colIndex(cols []string, name string) int {
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	return -1
}

func intFromCell(t *testing.T, v any) int {
	t.Helper()
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	default:
		t.Fatalf("not a number: %T %v", v, v)
		return 0
	}
}
