package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

const propEventstreamOperators = "operators"

// Operators sit between the Custom HTTP source and destinations. The Kafka
// topic stays the raw source (one DefaultStream); dests see the operator
// output. One micro-batch — the produce body — same class as the Lakehouse
// dest. Join / Union / Expand need more than one stream and stay refused.
//
// Window is tumbling over this batch only. Cross-batch window state would
// claim a streaming pipeline kustainer does not run.

type eventstreamOperator struct {
	Type       string                 `json:"type"`
	Condition  *eventstreamFilterCond `json:"condition,omitempty"`
	Keys       []string               `json:"keys,omitempty"`
	Aggregates []eventstreamAggregate `json:"aggregates,omitempty"`
	Kind       string                 `json:"kind,omitempty"`
	Duration   string                 `json:"duration,omitempty"`
	On         string                 `json:"on,omitempty"`
}

type eventstreamFilterCond struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

type eventstreamAggregate struct {
	Fn    string `json:"fn"`
	Field string `json:"field,omitempty"`
	As    string `json:"as"`
}

func loadEventstreamOperators(stored map[string]string) []eventstreamOperator {
	raw := stored[propEventstreamOperators]
	if raw == "" {
		return nil
	}
	var ops []eventstreamOperator
	if err := json.Unmarshal([]byte(raw), &ops); err != nil {
		return nil
	}
	return ops
}

func persistEventstreamOperators(st *store.Store, itemID string, ops []eventstreamOperator) error {
	raw, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	return st.SetItemProperties(itemID, map[string]string{
		propEventstreamOperators: string(raw),
	})
}

func (a *API) bindEventstreamOperator(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, stored, ok := a.eventstreamItem(w, r.PathValue("wid"), r.PathValue("iid"), store.RoleContributor, p)
	if !ok {
		return
	}
	var body eventstreamOperator
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "Malformed JSON body.")
		return
	}
	if err := validateEventstreamOperator(body); err != nil {
		writeErr(w, http.StatusBadRequest, "EventstreamOperatorNotSupported", err.Error())
		return
	}
	ops := loadEventstreamOperators(stored)
	ops = append(ops, body)
	if err := persistEventstreamOperators(a.Store, it.ID, ops); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, body)
}

func (a *API) listEventstreamOperators(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	_, stored, ok := a.eventstreamItem(w, r.PathValue("wid"), r.PathValue("iid"), store.RoleViewer, p)
	if !ok {
		return
	}
	ops := loadEventstreamOperators(stored)
	if ops == nil {
		ops = []eventstreamOperator{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": ops})
}

func validateEventstreamOperator(op eventstreamOperator) error {
	switch strings.TrimSpace(op.Type) {
	case "Filter":
		if op.Condition == nil || strings.TrimSpace(op.Condition.Field) == "" || strings.TrimSpace(op.Condition.Op) == "" {
			return fmt.Errorf("Filter needs condition.field and condition.op")
		}
		if _, err := normalizeFilterOp(op.Condition.Op); err != nil {
			return err
		}
		return nil
	case "GroupBy":
		if len(op.Aggregates) == 0 {
			return fmt.Errorf("GroupBy needs at least one aggregate")
		}
		for _, agg := range op.Aggregates {
			if strings.TrimSpace(agg.As) == "" {
				return fmt.Errorf("each aggregate needs as")
			}
			fn := strings.ToLower(strings.TrimSpace(agg.Fn))
			switch fn {
			case "count":
			case "sum", "min", "max", "avg":
				if strings.TrimSpace(agg.Field) == "" {
					return fmt.Errorf("%s needs field", fn)
				}
			default:
				return fmt.Errorf("aggregate %q is not count, sum, min, max, or avg", agg.Fn)
			}
		}
		return nil
	case "Window":
		if strings.ToLower(strings.TrimSpace(op.Kind)) != "tumbling" {
			return fmt.Errorf("Window kind must be tumbling (hopping and sliding need cross-batch state this wrap does not keep)")
		}
		if _, err := parseWindowDuration(op.Duration); err != nil {
			return err
		}
		return nil
	case "Join", "Union", "Expand":
		return fmt.Errorf("%s needs more than one stream, which this wrap does not host", op.Type)
	case "":
		return fmt.Errorf("type is required")
	default:
		return fmt.Errorf("operator type %s is not Filter, GroupBy, or Window", op.Type)
	}
}

func applyEventstreamOperators(ops []eventstreamOperator, events []struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}) ([]struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}, error) {
	out := events
	for _, op := range ops {
		var err error
		switch op.Type {
		case "Filter":
			out, err = applyFilter(op, out)
		case "Window":
			out, err = applyWindow(op, out)
		case "GroupBy":
			out, err = applyGroupBy(op, out)
		default:
			err = fmt.Errorf("operator type %s is not Filter, GroupBy, or Window", op.Type)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func eventObject(value string) (map[string]any, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(value), &obj); err != nil || obj == nil {
		return map[string]any{"value": value}, false
	}
	return obj, true
}

func applyFilter(op eventstreamOperator, events []struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}) ([]struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}, error) {
	opName, err := normalizeFilterOp(op.Condition.Op)
	if err != nil {
		return nil, err
	}
	var kept []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	for _, ev := range events {
		obj, _ := eventObject(ev.Value)
		if matchFilter(obj, op.Condition.Field, opName, op.Condition.Value) {
			kept = append(kept, ev)
		}
	}
	return kept, nil
}

func normalizeFilterOp(raw string) (string, error) {
	op := strings.ToLower(strings.TrimSpace(raw))
	switch op {
	case "eq", "ne", "gt", "gte", "lt", "lte", "contains", "exists":
		return op, nil
	default:
		return "", fmt.Errorf("Filter op %q is not eq, ne, gt, gte, lt, lte, contains, or exists", raw)
	}
}

func matchFilter(obj map[string]any, field, op string, want any) bool {
	got, ok := obj[field]
	if op == "exists" {
		return ok
	}
	if !ok {
		return false
	}
	switch op {
	case "eq":
		return filterEqual(got, want)
	case "ne":
		return !filterEqual(got, want)
	case "contains":
		return strings.Contains(fmt.Sprint(got), fmt.Sprint(want))
	case "gt", "gte", "lt", "lte":
		return filterCompare(got, want, op)
	}
	return false
}

func filterEqual(got, want any) bool {
	if gf, ok := asFloat(got); ok {
		if wf, ok := asFloat(want); ok {
			return gf == wf
		}
	}
	return fmt.Sprint(got) == fmt.Sprint(want)
}

func filterCompare(got, want any, op string) bool {
	gf, gok := asFloat(got)
	wf, wok := asFloat(want)
	if !gok || !wok {
		gs, ws := fmt.Sprint(got), fmt.Sprint(want)
		switch op {
		case "gt":
			return gs > ws
		case "gte":
			return gs >= ws
		case "lt":
			return gs < ws
		case "lte":
			return gs <= ws
		}
		return false
	}
	switch op {
	case "gt":
		return gf > wf
	case "gte":
		return gf >= wf
	case "lt":
		return gf < wf
	case "lte":
		return gf <= wf
	}
	return false
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func applyWindow(op eventstreamOperator, events []struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}) ([]struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}, error) {
	dur, err := parseWindowDuration(op.Duration)
	if err != nil {
		return nil, err
	}
	on := strings.TrimSpace(op.On)
	out := make([]struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}, 0, len(events))
	for _, ev := range events {
		obj, _ := eventObject(ev.Value)
		ts := windowTime(obj, on)
		start := ts.Truncate(dur).UTC().Format(time.RFC3339)
		obj["_window_start"] = start
		raw, err := json.Marshal(obj)
		if err != nil {
			return nil, err
		}
		ev.Value = string(raw)
		out = append(out, ev)
	}
	return out, nil
}

func parseWindowDuration(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("Window needs duration (for example 1m)")
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("Window duration %q is not a positive Go duration", raw)
	}
	return d, nil
}

func windowTime(obj map[string]any, on string) time.Time {
	if on != "" {
		if v, ok := obj[on]; ok {
			if t, ok := parseEventTime(v); ok {
				return t
			}
		}
	}
	for _, key := range []string{"timestamp", "ts", "time"} {
		if v, ok := obj[key]; ok {
			if t, ok := parseEventTime(v); ok {
				return t
			}
		}
	}
	return time.Unix(0, 0).UTC()
}

func parseEventTime(v any) (time.Time, bool) {
	switch x := v.(type) {
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, x); err == nil {
				return t, true
			}
		}
	}
	if f, ok := asFloat(v); ok {
		if f > 1e12 {
			return time.UnixMilli(int64(f)).UTC(), true
		}
		if f > 1e9 {
			return time.Unix(int64(f), 0).UTC(), true
		}
	}
	return time.Time{}, false
}

func applyGroupBy(op eventstreamOperator, events []struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}) ([]struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}, error) {
	type bucket struct {
		keys map[string]any
		n    int
		sum  map[string]float64
		min  map[string]float64
		max  map[string]float64
	}
	order := []string{}
	groups := map[string]*bucket{}
	for _, ev := range events {
		obj, _ := eventObject(ev.Value)
		keyParts := make([]string, len(op.Keys))
		keyVals := map[string]any{}
		for i, k := range op.Keys {
			v := obj[k]
			keyVals[k] = v
			raw, _ := json.Marshal(v)
			keyParts[i] = string(raw)
		}
		id := strings.Join(keyParts, "\x1e")
		b, ok := groups[id]
		if !ok {
			b = &bucket{
				keys: keyVals,
				sum:  map[string]float64{},
				min:  map[string]float64{},
				max:  map[string]float64{},
			}
			groups[id] = b
			order = append(order, id)
		}
		b.n++
		seenField := map[string]bool{}
		for _, agg := range op.Aggregates {
			fn := strings.ToLower(strings.TrimSpace(agg.Fn))
			if fn == "count" || strings.TrimSpace(agg.Field) == "" || seenField[agg.Field] {
				continue
			}
			seenField[agg.Field] = true
			f, ok := asFloat(obj[agg.Field])
			if !ok {
				continue
			}
			if _, seen := b.sum[agg.Field]; !seen {
				b.min[agg.Field] = f
				b.max[agg.Field] = f
			}
			b.sum[agg.Field] += f
			if f < b.min[agg.Field] {
				b.min[agg.Field] = f
			}
			if f > b.max[agg.Field] {
				b.max[agg.Field] = f
			}
		}
	}
	out := make([]struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}, 0, len(order))
	for _, id := range order {
		b := groups[id]
		row := map[string]any{}
		for k, v := range b.keys {
			row[k] = v
		}
		for _, agg := range op.Aggregates {
			fn := strings.ToLower(strings.TrimSpace(agg.Fn))
			switch fn {
			case "count":
				row[agg.As] = b.n
			case "sum":
				row[agg.As] = b.sum[agg.Field]
			case "min":
				row[agg.As] = b.min[agg.Field]
			case "max":
				row[agg.As] = b.max[agg.Field]
			case "avg":
				row[agg.As] = b.sum[agg.Field] / float64(b.n)
			}
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		out = append(out, struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}{Value: string(raw)})
	}
	return out, nil
}
