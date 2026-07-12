package pipeline

import (
	"encoding/json"
	"fmt"
)

// maxUntilIterations bounds an Until loop so a never-true condition fails the
// activity instead of hanging the interpreter.
const maxUntilIterations = 10000

// resolveField evaluates a definition field: a JSON string is an expression
// string; an object {"value":"@…","type":"Expression"} is the ADF expression
// wrapper; anything else is a literal value.
func resolveField(raw json.RawMessage, ctx *evalContext) (value, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return evalString(s, ctx)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		if _, isExpr := obj["type"]; isExpr {
			if vraw, ok := obj["value"]; ok {
				var vs string
				if json.Unmarshal(vraw, &vs) == nil {
					return evalString(vs, ctx)
				}
			}
		}
	}
	var v value
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// runOne executes a single activity and returns its terminal status + error.
func (r *run) runOne(a *Activity, item value, hasItem bool) (string, string) {
	ctx := r.ctx(item, hasItem)
	tp := map[string]json.RawMessage{}
	if len(a.TypeProperties) > 0 {
		_ = json.Unmarshal(a.TypeProperties, &tp)
	}
	resolve := func(raw json.RawMessage) (value, error) { return resolveField(raw, ctx) }

	switch a.Type {
	case "Wait":
		var w struct {
			WaitTimeInSeconds float64 `json:"waitTimeInSeconds"`
		}
		_ = json.Unmarshal(a.TypeProperties, &w)
		r.record(a, StatusSucceeded, nil, "", w.WaitTimeInSeconds)
		r.outputs[a.Name] = map[string]value{"status": StatusSucceeded}
		return StatusSucceeded, ""

	case "SetVariable":
		return r.setVariable(a, tp, resolve, false)
	case "AppendVariable":
		return r.setVariable(a, tp, resolve, true)

	case "IfCondition":
		return r.runIf(a, tp, item)
	case "Switch":
		return r.runSwitch(a, tp, resolve, item)
	case "ForEach":
		return r.runForEach(a, tp, resolve)
	case "Until":
		return r.runUntil(a, tp, item)

	case "Filter":
		return r.runFilter(a, tp, ctx)

	case "Fail":
		msg := "pipeline failed"
		if v, err := resolve(tp["message"]); err == nil && v != nil {
			msg = toString(v)
		}
		r.record(a, StatusFailed, nil, msg, 0)
		r.outputs[a.Name] = map[string]value{"status": StatusFailed}
		return StatusFailed, msg

	default:
		// Leaf / engine activity — delegate to the wired Executor.
		if r.exec == nil {
			err := fmt.Sprintf("no executor for activity type %q", a.Type)
			r.record(a, StatusFailed, nil, err, 0)
			r.outputs[a.Name] = map[string]value{"status": StatusFailed}
			return StatusFailed, err
		}
		out, err := r.exec.Execute(*a, resolve)
		if err != nil {
			r.record(a, StatusFailed, nil, err.Error(), 0)
			r.outputs[a.Name] = map[string]value{"status": StatusFailed}
			return StatusFailed, err.Error()
		}
		r.record(a, StatusSucceeded, out, "", 0)
		r.setOutput(a.Name, out)
		return StatusSucceeded, ""
	}
}

func (r *run) setVariable(a *Activity, tp map[string]json.RawMessage, resolve func(json.RawMessage) (value, error), appendMode bool) (string, string) {
	var name string
	_ = json.Unmarshal(tp["variableName"], &name)
	if name == "" {
		return r.fail(a, "variableName is required")
	}
	v, err := resolve(tp["value"])
	if err != nil {
		return r.fail(a, err.Error())
	}
	if appendMode {
		arr := toArray(r.variables[name])
		r.variables[name] = append(append([]value{}, arr...), v)
	} else {
		r.variables[name] = v
	}
	r.record(a, StatusSucceeded, map[string]any{"name": name, "value": r.variables[name]}, "", 0)
	r.outputs[a.Name] = map[string]value{"status": StatusSucceeded}
	return StatusSucceeded, ""
}

func (r *run) runIf(a *Activity, tp map[string]json.RawMessage, item value) (string, string) {
	cond, err := resolveField(tp["expression"], r.ctx(item, item != nil))
	if err != nil {
		return r.fail(a, err.Error())
	}
	branch := "ifFalseActivities"
	if toBool(cond) {
		branch = "ifTrueActivities"
	}
	return r.runContainer(a, tp[branch], item)
}

func (r *run) runSwitch(a *Activity, tp map[string]json.RawMessage, resolve func(json.RawMessage) (value, error), item value) (string, string) {
	on, err := resolve(tp["on"])
	if err != nil {
		return r.fail(a, err.Error())
	}
	var cases []struct {
		Value      string          `json:"value"`
		Activities json.RawMessage `json:"activities"`
	}
	_ = json.Unmarshal(tp["cases"], &cases)
	for _, c := range cases {
		if c.Value == toString(on) {
			return r.runContainer(a, c.Activities, item)
		}
	}
	return r.runContainer(a, tp["defaultActivities"], item)
}

func (r *run) runForEach(a *Activity, tp map[string]json.RawMessage, resolve func(json.RawMessage) (value, error)) (string, string) {
	items, err := resolve(tp["items"])
	if err != nil {
		return r.fail(a, err.Error())
	}
	arr := toArray(items)
	var inner []Activity
	if err := json.Unmarshal(tp["activities"], &inner); err != nil {
		return r.fail(a, "ForEach activities are invalid")
	}
	var failure string
	for _, el := range arr {
		if f := r.runActivities(inner, el); f != "" && failure == "" {
			failure = f
		}
	}
	return r.finishContainer(a, failure)
}

func (r *run) runUntil(a *Activity, tp map[string]json.RawMessage, item value) (string, string) {
	var inner []Activity
	if err := json.Unmarshal(tp["activities"], &inner); err != nil {
		return r.fail(a, "Until activities are invalid")
	}
	for i := 0; i < maxUntilIterations; i++ {
		if f := r.runActivities(inner, item); f != "" {
			return r.finishContainer(a, f)
		}
		cond, err := resolveField(tp["expression"], r.ctx(item, item != nil))
		if err != nil {
			return r.fail(a, err.Error())
		}
		if toBool(cond) {
			return r.finishContainer(a, "")
		}
	}
	return r.fail(a, fmt.Sprintf("Until did not converge within %d iterations", maxUntilIterations))
}

// runFilter is a native array transform: output {"value": [items matching condition]}.
func (r *run) runFilter(a *Activity, tp map[string]json.RawMessage, ctx *evalContext) (string, string) {
	items, err := resolveField(tp["items"], ctx)
	if err != nil {
		return r.fail(a, err.Error())
	}
	var out []value
	for _, el := range toArray(items) {
		ictx := r.ctx(el, true)
		keep, err := resolveField(tp["condition"], ictx)
		if err != nil {
			return r.fail(a, err.Error())
		}
		if toBool(keep) {
			out = append(out, el)
		}
	}
	output := map[string]any{"value": out}
	r.record(a, StatusSucceeded, output, "", 0)
	r.setOutput(a.Name, output)
	return StatusSucceeded, ""
}

// runContainer runs a nested activity list (an If branch or Switch case) and
// records the container's aggregate status.
func (r *run) runContainer(a *Activity, raw json.RawMessage, item value) (string, string) {
	var inner []Activity
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &inner)
	}
	failure := r.runActivities(inner, item)
	return r.finishContainer(a, failure)
}

func (r *run) finishContainer(a *Activity, failure string) (string, string) {
	if failure != "" {
		r.record(a, StatusFailed, nil, failure, 0)
		r.outputs[a.Name] = map[string]value{"status": StatusFailed}
		return StatusFailed, failure
	}
	r.record(a, StatusSucceeded, nil, "", 0)
	r.outputs[a.Name] = map[string]value{"status": StatusSucceeded}
	return StatusSucceeded, ""
}

func (r *run) fail(a *Activity, msg string) (string, string) {
	r.record(a, StatusFailed, nil, msg, 0)
	r.outputs[a.Name] = map[string]value{"status": StatusFailed}
	return StatusFailed, msg
}
