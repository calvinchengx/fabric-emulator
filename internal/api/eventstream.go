package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

const (
	propEventstreamDatasource = "datasourceId"
	propEventstreamTopic      = "kafkaTopic"
	defaultStreamName         = "DefaultStream"
)

// registerEventstream mounts the Spark-facing resolve route (item GUID, no
// workspace — Fabric's notebook adapter has only eventstream.itemid +
// eventstream.datasourceid) and the Custom HTTP produce path.
func (a *API) registerEventstream(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/eventstreams/{iid}/sources/{did}", a.withAuth(a.resolveEventstreamSource))
	mux.HandleFunc("GET /v1/eventstreams/{iid}/sources/{did}/events", a.withAuth(a.consumeEventstreamEvents))
	mux.HandleFunc("POST /v1/workspaces/{wid}/eventstreams/{iid}/sources/{did}/events",
		a.withAuth(a.produceEventstreamEvents))
}

func eventstreamTopic(itemID, datasourceID string) string {
	return itemID + "." + datasourceID
}

// provisionEventstream mints a DefaultStream datasourceId and a Kafka topic
// name on create. Topic creation against the broker is best-effort: management
// must succeed without a broker (same grammar as Eventhouse without kustainer).
func (a *API) provisionEventstream(it *store.Item) {
	ds := store.NewID()
	topic := eventstreamTopic(it.ID, ds)
	_ = a.Store.SetItemProperties(it.ID, map[string]string{
		propEventstreamDatasource: ds,
		propEventstreamTopic:      topic,
	})
	if a.Kafka == nil {
		return
	}
	if err := a.Kafka.CreateTopic(topic); err != nil {
		log.Printf("eventstream: CreateTopics %s: %v (topic will auto-create on first produce)", topic, err)
	}
}

func (a *API) eventstreamProperties(it *store.Item) map[string]any {
	stored, err := a.Store.ItemProperties(it.ID)
	if err != nil || stored[propEventstreamDatasource] == "" {
		return nil
	}
	return map[string]any{
		"streams": []map[string]any{{
			"id":   stored[propEventstreamDatasource],
			"name": defaultStreamName,
			"type": defaultStreamName,
		}},
	}
}

type eventstreamSource struct {
	ItemID           string `json:"itemId"`
	DatasourceID     string `json:"datasourceId"`
	BootstrapServers string `json:"bootstrapServers"`
	Topic            string `json:"topic"`
}

// resolveEventstreamSource is what the JVM Spark adapter calls with the
// notebook's Entra token. Unknown IDs 404; no broker 501. Never an empty stream.
func (a *API) resolveEventstreamSource(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, stored, ok := a.eventstreamLookup(w, p, r.PathValue("iid"), r.PathValue("did"), store.RoleViewer)
	if !ok {
		return
	}
	if a.KafkaBootstrap == "" {
		writeErr(w, http.StatusNotImplemented, "KafkaBrokerNotAttached",
			"No Kafka broker is attached: start the emulator with --kafka-bootstrap (FABRIC_KAFKA_BOOTSTRAP) pointing at one.")
		return
	}
	writeJSON(w, http.StatusOK, eventstreamSource{
		ItemID:           it.ID,
		DatasourceID:     stored[propEventstreamDatasource],
		BootstrapServers: a.KafkaBootstrap,
		Topic:            stored[propEventstreamTopic],
	})
}

type eventstreamEventBatch struct {
	Events []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"events"`
}

// produceEventstreamEvents is the one Custom source this slice implements:
// JSON key/value bytes into the item's topic. Not thirty connectors.
func (a *API) produceEventstreamEvents(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil || it.Type != "Eventstream" {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return
	}
	_, stored, ok := a.eventstreamLookup(w, p, it.ID, r.PathValue("did"), store.RoleContributor)
	if !ok {
		return
	}
	if a.Kafka == nil {
		writeErr(w, http.StatusNotImplemented, "KafkaBrokerNotAttached",
			"No Kafka broker is attached: start the emulator with --kafka-bootstrap (FABRIC_KAFKA_BOOTSTRAP) pointing at one.")
		return
	}
	var body eventstreamEventBatch
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Events) == 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "events is required.")
		return
	}
	topic := stored[propEventstreamTopic]
	for _, ev := range body.Events {
		if err := a.Kafka.Produce(topic, []byte(ev.Key), []byte(ev.Value)); err != nil {
			writeErr(w, http.StatusBadGateway, "KafkaProduceFailed", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"produced": len(body.Events), "topic": topic})
}

// consumeEventstreamEvents is the Sail path: the agent has no Kafka client, so
// the control plane reads the topic (from earliest, one bounded pull) and
// returns Kafka-shaped records. Unknown IDs 404; no broker 501.
func (a *API) consumeEventstreamEvents(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	_, stored, ok := a.eventstreamLookup(w, p, r.PathValue("iid"), r.PathValue("did"), store.RoleViewer)
	if !ok {
		return
	}
	if a.Kafka == nil {
		writeErr(w, http.StatusNotImplemented, "KafkaBrokerNotAttached",
			"No Kafka broker is attached: start the emulator with --kafka-bootstrap (FABRIC_KAFKA_BOOTSTRAP) pointing at one.")
		return
	}
	max := 100
	if v := r.URL.Query().Get("max"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "InvalidRequest", "max must be a positive integer.")
			return
		}
		if n > 1000 {
			n = 1000
		}
		max = n
	}
	wait := 5 * time.Second
	if v := r.URL.Query().Get("timeoutMs"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "InvalidRequest", "timeoutMs must be a positive integer.")
			return
		}
		if n > 30000 {
			n = 30000
		}
		wait = time.Duration(n) * time.Millisecond
	}
	topic := stored[propEventstreamTopic]
	recs, err := a.Kafka.Consume(topic, max, wait)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "KafkaConsumeFailed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": recs, "topic": topic})
}

func (a *API) eventstreamLookup(w http.ResponseWriter, p *auth.Principal, itemID, datasourceID, minRole string) (*store.Item, map[string]string, bool) {
	it, err := a.Store.GetItemByID(itemID)
	if err != nil || it.Type != "Eventstream" {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The Eventstream item is not available.")
		return nil, nil, false
	}
	if _, _, ok := a.requireRole(w, it.WorkspaceID, p, minRole); !ok {
		return nil, nil, false
	}
	stored, err := a.Store.ItemProperties(it.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return nil, nil, false
	}
	if stored[propEventstreamDatasource] == "" || stored[propEventstreamDatasource] != datasourceID {
		writeErr(w, http.StatusNotFound, "EventstreamSourceNotFound",
			"The Eventstream datasource id is not available.")
		return nil, nil, false
	}
	return it, stored, true
}
