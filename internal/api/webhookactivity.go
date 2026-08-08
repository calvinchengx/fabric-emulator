package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// The WebHook activity, for real: call the endpoint with a callBackUri, then
// PARK until the receiver calls back — or the timeout, measured on the
// VIRTUAL clock.
//
// The oracle is ADF's published schema (the same entityTypes/Pipeline.json as
// Functions): discriminator `WebHook`; `method` an enum of exactly POST;
// `url`; `timeout` in D.HH:MM:SS defaulting to ten minutes; `headers`;
// `body`; `reportStatusOnCallBack`. The defining half — the park — was
// impossible while pipelines executed inline in the job POST, which is why
// this activity spent one release as a loud refusal pointing at doc 37 §4.
// Async pipelines exist now, so the refusal comes out in the same change its
// replacement goes in.
//
// The callBackUri is a PATH, not an absolute URL, on the repo's own precedent
// (e2e/rest-helix: "Fabric's contract is the path; the scheme is this
// deployment's business") — the emulator does not know the base URL a caller
// reaches it on, and advertising a wrong absolute would be worse than
// advertising none. A receiver prefixes the base it already used to reach the
// emulator. The token in the path is the credential, exactly as ADF's
// callBackUri embeds its own — which is why the callback route takes no
// bearer: an external receiver has no Fabric token, and possession of the
// exact URI is the authentication.
//
// TIMEOUT SEMANTICS. The deadline lives on the store's clock: a frozen clock
// advanced past it MUST expire the park (no real sleeps — the repo's rule),
// and an unfrozen clock expires it in real time because virtual time tracks
// real time. The park therefore selects on three things: the callback, a
// clock-change notification (clock.Changed), and a real-time timer re-armed
// from the remaining VIRTUAL duration after every wake. A restart does not
// resume a parked pipeline; the goroutine dies with the process and the job
// stays InProgress, which is the honest answer for an emulator whose runs are
// in-memory.

const webhookDefaultTimeout = 10 * time.Minute

// webhookCallback is what the callback route delivers to a parked activity.
type webhookCallback struct {
	body map[string]any
}

func (e *pipelineExecutor) webhookActivity(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	method := "POST"
	if raw, ok := tp["method"]; ok && len(raw) > 0 {
		v, err := resolve(raw)
		if err != nil {
			return nil, fmt.Errorf("webhook activity %q: method: %w", act.Name, err)
		}
		if s := strings.ToUpper(strings.TrimSpace(fmt.Sprint(v))); s != "" {
			method = s
		}
	}
	if method != "POST" {
		// The schema's enum holds exactly POST; accepting another verb would
		// certify a definition Fabric rejects.
		return nil, fmt.Errorf("webhook activity %q: method %q is not in the activity's enum (POST only)", act.Name, method)
	}

	rawURL := ""
	if raw, ok := tp["url"]; ok && len(raw) > 0 {
		v, err := resolve(raw)
		if err != nil {
			return nil, fmt.Errorf("webhook activity %q: url: %w", act.Name, err)
		}
		rawURL = strings.TrimSpace(fmt.Sprint(v))
	}
	if rawURL == "" {
		return nil, fmt.Errorf("webhook activity %q: url is required", act.Name)
	}

	// The body must be a JSON OBJECT (or absent): callBackUri is injected into
	// it, ADF's own mechanism, and a non-object gives it nowhere to ride.
	bodyObj := map[string]any{}
	if raw, ok := tp["body"]; ok && len(raw) > 0 {
		v, err := resolve(raw)
		if err != nil {
			return nil, fmt.Errorf("webhook activity %q: body: %w", act.Name, err)
		}
		if v != nil {
			obj, isObj := v.(map[string]any)
			if !isObj {
				return nil, fmt.Errorf("webhook activity %q: body must be a JSON object — "+
					"callBackUri is injected into it and a %T gives it nowhere to ride", act.Name, v)
			}
			bodyObj = obj
		}
	}

	timeout := webhookDefaultTimeout
	if raw, ok := tp["timeout"]; ok && len(raw) > 0 {
		v, err := resolve(raw)
		if err != nil {
			return nil, fmt.Errorf("webhook activity %q: timeout: %w", act.Name, err)
		}
		if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
			d, ok := pipeline.ParseTimeout(s)
			if !ok || d <= 0 {
				return nil, fmt.Errorf("webhook activity %q: timeout %q is not D.HH:MM:SS", act.Name, s)
			}
			timeout = d
		}
	}

	reportStatus := false
	if raw, ok := tp["reportStatusOnCallBack"]; ok && len(raw) > 0 {
		if v, err := resolve(raw); err == nil {
			reportStatus, _ = v.(bool)
		}
	}

	headers := map[string]string{}
	if raw, ok := tp["headers"]; ok && len(raw) > 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("webhook activity %q: headers are not an object", act.Name)
		}
		for name, vraw := range fields {
			v, herr := resolve(vraw)
			if herr != nil {
				return nil, fmt.Errorf("webhook activity %q: header %q: %w", act.Name, name, herr)
			}
			headers[name] = fmt.Sprint(v)
		}
	}

	// Register the park BEFORE the call: a receiver that calls back inline,
	// before the initial POST even returns, must find the waiter present.
	token := store.NewID()
	ch := make(chan webhookCallback, 1)
	e.a.webhookWaits.Store(token, ch)
	defer e.a.webhookWaits.Delete(token)

	bodyObj["callBackUri"] = fmt.Sprintf(
		"/v1/workspaces/%s/items/%s/jobs/instances/%s/webhookcallbacks/%s",
		e.wid, e.chain[0], e.jobID, token)

	if _, err := e.httpActivity("webhook", act, method, rawURL, headers, bodyObj); err != nil {
		return nil, err
	}

	// The park. Deadline on the virtual clock; three wake sources, and the
	// real-time timer is re-armed from the REMAINING virtual duration each
	// wake, because an Advance can consume most of it without any real time
	// passing.
	deadline := e.a.Store.Now() + int64(timeout/time.Second)
	for {
		remaining := deadline - e.a.Store.Now()
		if remaining <= 0 {
			return nil, fmt.Errorf("webhook activity %q: no callback within the timeout — "+
				"the receiver never called the callBackUri back", act.Name)
		}
		clockChanged := e.a.Store.Clock.Changed()
		select {
		case cb := <-ch:
			out := map[string]any{"status": "Succeeded"}
			for k, v := range cb.body {
				out[k] = v
			}
			// reportStatusOnCallBack: the schema's words — "statusCode, output
			// and error in callback request body will be consumed by activity".
			// A reported statusCode outside 2xx fails the activity with the
			// reported error.
			if reportStatus {
				if sc, ok := asInt(cb.body["statusCode"]); ok && (sc < 200 || sc > 299) {
					msg := fmt.Sprint(cb.body["error"])
					if msg == "<nil>" || msg == "" {
						msg = fmt.Sprintf("callback reported statusCode %d", sc)
					}
					return nil, fmt.Errorf("webhook activity %q: %s", act.Name, msg)
				}
			}
			return out, nil
		case <-clockChanged:
			// Re-check the deadline against the moved clock.
		case <-time.After(time.Duration(remaining) * time.Second):
			// Real time caught up with the virtual deadline.
		}
	}
}

// asInt reads a JSON number (float64 after Unmarshal) or int as an int.
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

// webhookCallbackHandler resumes a parked WebHook activity. No bearer: the
// token in the path is the credential, as ADF's own callBackUri embeds its —
// an external receiver has no Fabric token, and possession of the exact URI
// is the authentication. An unknown or already-consumed token is a plain 404
// that reveals nothing about which jobs exist.
func (a *API) webhookCallbackHandler(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	v, ok := a.webhookWaits.LoadAndDelete(token)
	if !ok {
		writeErr(w, http.StatusNotFound, "WebhookCallbackNotFound", "No activity is awaiting this callback.")
		return
	}
	body := map[string]any{}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	v.(chan webhookCallback) <- webhookCallback{body: body}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}
