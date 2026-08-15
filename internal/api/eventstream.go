package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

const (
	propEventstreamDatasource   = "datasourceId"
	propEventstreamTopic        = "kafkaTopic"
	propEventstreamDestinations = "destinations"
	defaultStreamName           = "DefaultStream"
)

// registerEventstream mounts the Spark-facing resolve route (item GUID, no
// workspace — Fabric's notebook adapter has only eventstream.itemid +
// eventstream.datasourceid), the Custom HTTP produce path, and the
// emulator-native destination bind (Fabric's topology has no public REST).
func (a *API) registerEventstream(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/eventstreams/{iid}/sources/{did}", a.withAuth(a.resolveEventstreamSource))
	mux.HandleFunc("GET /v1/eventstreams/{iid}/sources/{did}/events", a.withAuth(a.consumeEventstreamEvents))
	mux.HandleFunc("POST /v1/workspaces/{wid}/eventstreams/{iid}/sources/{did}/events",
		a.withAuth(a.produceEventstreamEvents))
	mux.HandleFunc("POST /v1/workspaces/{wid}/eventstreams/{iid}/destinations",
		a.withAuth(a.bindEventstreamDestination))
	mux.HandleFunc("GET /v1/workspaces/{wid}/eventstreams/{iid}/destinations",
		a.withAuth(a.listEventstreamDestinations))
	mux.HandleFunc("POST /v1/workspaces/{wid}/eventstreams/{iid}/operators",
		a.withAuth(a.bindEventstreamOperator))
	mux.HandleFunc("GET /v1/workspaces/{wid}/eventstreams/{iid}/operators",
		a.withAuth(a.listEventstreamOperators))
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
	props := map[string]any{
		"streams": []map[string]any{{
			"id":   stored[propEventstreamDatasource],
			"name": defaultStreamName,
			"type": defaultStreamName,
		}},
	}
	if dests := loadEventstreamDestinations(stored); len(dests) > 0 {
		props["destinations"] = dests
	}
	if ops := loadEventstreamOperators(stored); len(ops) > 0 {
		props["operators"] = ops
	}
	return props
}

type eventstreamDestination struct {
	Type        string `json:"type"`
	ItemID      string `json:"itemId"`
	Table       string `json:"table"`
	Database    string `json:"database,omitempty"`
	WorkspaceID string `json:"workspaceId,omitempty"`
}

func loadEventstreamDestinations(stored map[string]string) []eventstreamDestination {
	raw := stored[propEventstreamDestinations]
	if raw == "" {
		return nil
	}
	var dests []eventstreamDestination
	if err := json.Unmarshal([]byte(raw), &dests); err != nil {
		return nil
	}
	return dests
}

func persistEventstreamDestinations(st *store.Store, itemID string, dests []eventstreamDestination) error {
	raw, err := json.Marshal(dests)
	if err != nil {
		return err
	}
	return st.SetItemProperties(itemID, map[string]string{
		propEventstreamDestinations: string(raw),
	})
}

func (a *API) eventstreamItem(w http.ResponseWriter, wid, iid, minRole string, p *auth.Principal) (*store.Item, map[string]string, bool) {
	if _, _, ok := a.requireRole(w, wid, p, minRole); !ok {
		return nil, nil, false
	}
	it, err := a.Store.GetItem(wid, iid)
	if err != nil || it.Type != "Eventstream" {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return nil, nil, false
	}
	stored, err := a.Store.ItemProperties(it.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return nil, nil, false
	}
	return it, stored, true
}

func (a *API) bindEventstreamDestination(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, stored, ok := a.eventstreamItem(w, r.PathValue("wid"), r.PathValue("iid"), store.RoleContributor, p)
	if !ok {
		return
	}
	var body eventstreamDestination
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "Malformed JSON body.")
		return
	}
	switch body.Type {
	case "Lakehouse", "Reflex", "Eventhouse":
		// bound below
	case "":
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "type is required.")
		return
	default:
		writeErr(w, http.StatusBadRequest, "EventstreamDestinationTypeNotSupported",
			"Eventstream destination type "+body.Type+" is not supported.")
		return
	}
	if body.ItemID == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "itemId is required.")
		return
	}
	switch body.Type {
	case "Lakehouse":
		table := strings.TrimSpace(body.Table)
		if table == "" || strings.Contains(table, "/") {
			writeErr(w, http.StatusBadRequest, "InvalidRequest",
				"table must be a single Tables/ segment (no slashes).")
			return
		}
		body.Table = table
		body.Database = ""
	case "Eventhouse":
		table := strings.TrimSpace(body.Table)
		if !kustoTableNameOK(table) {
			writeErr(w, http.StatusBadRequest, "InvalidRequest",
				"table must be a Kusto table name ([A-Za-z_][A-Za-z0-9_]*).")
			return
		}
		body.Table = table
		body.Database = strings.TrimSpace(body.Database)
	default:
		body.Table = ""
		body.Database = ""
	}
	destWS := body.WorkspaceID
	if destWS == "" {
		destWS = it.WorkspaceID
	} else if destWS != it.WorkspaceID {
		if _, _, ok := a.requireRole(w, destWS, p, store.RoleContributor); !ok {
			return
		}
	}
	dest, err := a.Store.GetItem(destWS, body.ItemID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The destination item is not available.")
		return
	}
	if dest.Type != body.Type {
		writeErr(w, http.StatusBadRequest, "EventstreamDestinationNot"+body.Type,
			"itemId must name a "+body.Type+".")
		return
	}
	body.WorkspaceID = destWS
	dests := loadEventstreamDestinations(stored)
	for _, d := range dests {
		if d.Type == body.Type && d.ItemID == body.ItemID && d.Table == body.Table && d.WorkspaceID == body.WorkspaceID {
			writeJSON(w, http.StatusCreated, body)
			return
		}
	}
	dests = append(dests, body)
	if err := persistEventstreamDestinations(a.Store, it.ID, dests); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, body)
}

func (a *API) listEventstreamDestinations(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	_, stored, ok := a.eventstreamItem(w, r.PathValue("wid"), r.PathValue("iid"), store.RoleViewer, p)
	if !ok {
		return
	}
	dests := loadEventstreamDestinations(stored)
	if dests == nil {
		dests = []eventstreamDestination{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": dests})
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
	drained, err := applyEventstreamOperators(loadEventstreamOperators(stored), body.Events)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "EventstreamOperatorFailed", err.Error())
		return
	}
	out := eventstreamEventBatch{Events: drained}
	if err := a.drainEventstreamToLakehouse(it, stored, out); err != nil {
		writeErr(w, http.StatusBadGateway, "EventstreamDestinationWriteFailed", err.Error())
		return
	}
	if err := a.drainEventstreamToReflex(it, stored, out); err != nil {
		writeErr(w, http.StatusBadGateway, "EventstreamDestinationReflexFailed", err.Error())
		return
	}
	if err := a.drainEventstreamToEventhouse(it, stored, out); err != nil {
		writeErr(w, http.StatusBadGateway, "EventstreamDestinationEventhouseFailed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"produced": len(body.Events),
		"drained":  len(drained),
		"topic":    topic,
	})
}

func (a *API) drainEventstreamToReflex(it *store.Item, stored map[string]string, body eventstreamEventBatch) error {
	dests := loadEventstreamDestinations(stored)
	for _, d := range dests {
		if d.Type != "Reflex" {
			continue
		}
		destWS := d.WorkspaceID
		if destWS == "" {
			destWS = it.WorkspaceID
		}
		reflex, err := a.Store.GetItem(destWS, d.ItemID)
		if err != nil {
			return err
		}
		if reflex.Type != "Reflex" {
			return fmt.Errorf("destination item %s is not a Reflex", d.ItemID)
		}
		triggers, err := a.Store.ListEventTriggers(reflex.ID)
		if err != nil {
			return err
		}
		for _, ev := range body.Events {
			params := streamTriggerEventParams(it, ev.Key, ev.Value)
			for _, t := range triggers {
				if !t.Enabled || t.EventType != store.EventEventstreamReceived || t.SourceItemID != it.ID {
					continue
				}
				if !a.firing.enter(t.ID) {
					continue
				}
				ok := a.fireTriggerWith(t, params)
				a.firing.leave(t.ID)
				if !ok {
					return fmt.Errorf("reflex destination %s did not start %s", reflex.ID, t.TargetItemID)
				}
			}
		}
	}
	return nil
}

func streamTriggerEventParams(es *store.Item, key, value string) map[string]any {
	return map[string]any{
		"EventType":   store.EventEventstreamReceived,
		"Source":      es.ID,
		"Subject":     es.ID,
		"Key":         key,
		"Value":       value,
		"WorkspaceId": es.WorkspaceID,
		"ItemId":      es.ID,
	}
}

func (a *API) drainEventstreamToLakehouse(it *store.Item, stored map[string]string, body eventstreamEventBatch) error {
	dests := loadEventstreamDestinations(stored)
	if len(dests) == 0 {
		return nil
	}
	tbl := eventsToDeltaTable(body)
	if tbl == nil {
		return nil
	}
	attr := store.Attribution{JobID: it.ID, ActivityName: "Eventstream"}
	for _, d := range dests {
		if d.Type != "Lakehouse" {
			continue
		}
		destWS := d.WorkspaceID
		if destWS == "" {
			destWS = it.WorkspaceID
		}
		if err := warehouse.WriteDeltaTableAs(attr, a.Store, destWS, d.ItemID, d.Table, warehouse.WriteAppend, tbl); err != nil {
			return err
		}
	}
	return nil
}

func (a *API) drainEventstreamToEventhouse(it *store.Item, stored map[string]string, body eventstreamEventBatch) error {
	dests := loadEventstreamDestinations(stored)
	for _, d := range dests {
		if d.Type != "Eventhouse" {
			continue
		}
		destWS := d.WorkspaceID
		if destWS == "" {
			destWS = it.WorkspaceID
		}
		eh, err := a.Store.GetItem(destWS, d.ItemID)
		if err != nil {
			return err
		}
		if eh.Type != "Eventhouse" {
			return fmt.Errorf("destination item %s is not an Eventhouse", d.ItemID)
		}
		if a.KQLURL == nil {
			return fmt.Errorf("no Kusto engine is attached: start the emulator with --kql-url (FABRIC_KQL_URL) pointing at one")
		}
		db, err := a.resolveKQLDatabase(destWS, eh.ID, d.Database)
		if err != nil {
			return err
		}
		tbl := eventsToDeltaTable(body)
		if tbl == nil {
			continue
		}
		engineDB := engineDatabaseName(db.ID)
		ctx := context.Background()
		if err := a.ensureKustoDatabase(ctx, engineDB); err != nil {
			return err
		}
		if err := a.kustoIngestTable(ctx, engineDB, d.Table, tbl); err != nil {
			return err
		}
	}
	return nil
}

func (a *API) kustoIngestTable(ctx context.Context, engineDB, table string, tbl *warehouse.Table) error {
	cols := make([]string, len(tbl.Columns))
	types := make([]string, len(tbl.Columns))
	for i, name := range tbl.Columns {
		if !kustoTableNameOK(name) {
			return fmt.Errorf("column %q is not a Kusto identifier", name)
		}
		cols[i] = name
		for _, row := range tbl.Rows {
			if i < len(row) {
				types[i] = kustoWiden(types[i], row[i])
			}
		}
		if types[i] == "" {
			types[i] = "string"
		}
	}
	var b strings.Builder
	b.WriteString(".create-merge table ")
	b.WriteString(table)
	b.WriteString(" (")
	for i, name := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(types[i])
	}
	b.WriteByte(')')
	if err := a.kustoMgmt(ctx, engineDB, b.String()); err != nil {
		return err
	}
	var rows strings.Builder
	rows.WriteString(".ingest inline into table ")
	rows.WriteString(table)
	rows.WriteString(" <|")
	for _, row := range tbl.Rows {
		rows.WriteByte('\n')
		for i := range cols {
			if i > 0 {
				rows.WriteByte(',')
			}
			var cell any
			if i < len(row) {
				cell = row[i]
			}
			rows.WriteString(kustoCSV(cell))
		}
	}
	return a.kustoMgmt(ctx, engineDB, rows.String())
}

func (a *API) kustoMgmt(ctx context.Context, engineDB, csl string) error {
	status, payload, err := a.callKusto(ctx, "v1", "mgmt", kustoRequest{DB: engineDB, CSL: csl})
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("the Kusto engine returned %d: %s", status, truncate(payload))
	}
	return nil
}

func kustoTableNameOK(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		ok := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
			(i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

func kustoWiden(cur string, v any) string {
	next := ""
	switch v.(type) {
	case nil:
		return cur
	case bool:
		next = "bool"
	case float64, float32, int, int32, int64:
		next = "real"
	default:
		next = "string"
	}
	if cur == "" || cur == next {
		return next
	}
	return "string"
}

func kustoCSV(v any) string {
	if v == nil {
		return ""
	}
	s := fmt.Sprint(v)
	if strings.ContainsAny(s, ",\"\n\r") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func eventsToDeltaTable(body eventstreamEventBatch) *warehouse.Table {
	if len(body.Events) == 0 {
		return nil
	}
	parsed := make([]map[string]any, len(body.Events))
	allObjects := true
	for i, ev := range body.Events {
		var obj map[string]any
		if err := json.Unmarshal([]byte(ev.Value), &obj); err != nil || obj == nil {
			allObjects = false
			break
		}
		parsed[i] = obj
	}
	if !allObjects {
		rows := make([][]any, len(body.Events))
		for i, ev := range body.Events {
			rows[i] = []any{ev.Value}
		}
		return &warehouse.Table{Columns: []string{"value"}, Rows: rows}
	}
	keys := map[string]struct{}{}
	for _, obj := range parsed {
		for k := range obj {
			keys[k] = struct{}{}
		}
	}
	cols := make([]string, 0, len(keys))
	for k := range keys {
		cols = append(cols, k)
	}
	sort.Strings(cols)
	rows := make([][]any, len(parsed))
	for i, obj := range parsed {
		row := make([]any, len(cols))
		for j, c := range cols {
			row[j] = deltaCell(obj[c])
		}
		rows[i] = row
	}
	return &warehouse.Table{Columns: cols, Rows: rows}
}

func deltaCell(v any) any {
	switch x := v.(type) {
	case nil, bool, string, float64, float32, int, int32, int64:
		return x
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return nil
		}
		return string(b)
	}
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
