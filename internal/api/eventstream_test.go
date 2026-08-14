package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
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
