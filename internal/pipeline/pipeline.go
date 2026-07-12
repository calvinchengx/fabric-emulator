// Package pipeline is a faithful interpreter for Microsoft Fabric / ADF Data
// Pipeline definitions: control flow (If/ForEach/Until/Switch), variables, the
// expression language, and dependency conditions — the orchestration semantics
// a pipeline encodes. Leaf "engine" activities (notebook runs, copies) are
// delegated to an Executor the caller wires to the real engines; the emulator's
// job is to run the orchestration correctly, not to re-host every engine.
//
// Pure Go (no CGO), so it lives in the binary under the project's constraints.
package pipeline

import (
	"encoding/json"
	"fmt"
)

// Terminal activity/pipeline statuses (Fabric's spelling).
const (
	StatusSucceeded = "Succeeded"
	StatusFailed    = "Failed"
	StatusSkipped   = "Skipped"
)

// Activity is one node of a pipeline. TypeProperties is decoded per type.
type Activity struct {
	Name           string          `json:"name"`
	Type           string          `json:"type"`
	DependsOn      []Dependency    `json:"dependsOn"`
	Policy         *Policy         `json:"policy,omitempty"`
	TypeProperties json.RawMessage `json:"typeProperties"`
}

// Policy is an activity's execution policy — Fabric's per-activity General
// settings (activity-overview.md): a retry count with a backoff interval and a
// wall-clock timeout. Applied uniformly to every activity type by runWithPolicy.
type Policy struct {
	Timeout                string  `json:"timeout"`                // "D.HH:MM:SS" (default 12h in Fabric; unset here = none)
	Retry                  int     `json:"retry"`                  // extra attempts after the first failure (Fabric: 1–1000)
	RetryIntervalInSeconds float64 `json:"retryIntervalInSeconds"` // virtual backoff between attempts (default 30 in Fabric)
}

// Dependency gates an activity on an upstream one ending in one of the
// listed conditions (Succeeded/Failed/Completed/Skipped).
type Dependency struct {
	Activity             string   `json:"activity"`
	DependencyConditions []string `json:"dependencyConditions"`
}

type paramDef struct {
	Type         string `json:"type"`
	DefaultValue value  `json:"defaultValue"`
}

// Pipeline is the parsed definition (the pipeline-content.json payload).
type Pipeline struct {
	Properties struct {
		Activities []Activity          `json:"activities"`
		Parameters map[string]paramDef `json:"parameters"`
		Variables  map[string]paramDef `json:"variables"`
	} `json:"properties"`
}

// ActivityRun is the recorded outcome of one activity (Fabric's activity-run
// shape, trimmed): the queryactivityruns surface returns these.
type ActivityRun struct {
	Name     string         `json:"activityName"`
	Type     string         `json:"activityType"`
	Status   string         `json:"status"`
	Output   map[string]any `json:"output,omitempty"`
	Error    string         `json:"error,omitempty"`
	Duration float64        `json:"durationInSeconds,omitempty"`
	Retry    int            `json:"retryAttempt,omitempty"` // attempts consumed by policy.retry before this outcome
}

// Result is the whole run.
type Result struct {
	Status     string
	Activities []ActivityRun
	Variables  map[string]value
	Error      string
}

// Executor runs a leaf/engine activity (anything the interpreter doesn't handle
// natively). It returns the activity's output object or an error (which fails
// the activity). resolve evaluates an expression field against the live scope.
type Executor interface {
	Execute(act Activity, resolve func(json.RawMessage) (value, error)) (map[string]any, error)
}

// Parse decodes a pipeline definition payload.
func Parse(def []byte) (*Pipeline, error) {
	var p Pipeline
	if err := json.Unmarshal(def, &p); err != nil {
		return nil, fmt.Errorf("invalid pipeline definition: %w", err)
	}
	return &p, nil
}

type run struct {
	exec      Executor
	params    map[string]value
	variables map[string]value
	outputs   map[string]value // activity name -> {"output":..,"status":..}
	runs      []ActivityRun
}

// Run executes the pipeline with the given runtime parameters (overriding
// defaults). The Executor handles leaf activities.
func (p *Pipeline) Run(params map[string]value, exec Executor) *Result {
	r := &run{
		exec:      exec,
		params:    map[string]value{},
		variables: map[string]value{},
		outputs:   map[string]value{},
	}
	for name, def := range p.Properties.Parameters {
		r.params[name] = def.DefaultValue
	}
	for name, v := range params {
		r.params[name] = v
	}
	for name, def := range p.Properties.Variables {
		if def.DefaultValue != nil {
			r.variables[name] = def.DefaultValue
		} else {
			r.variables[name] = ""
		}
	}
	failed := r.runActivities(p.Properties.Activities, nil)
	res := &Result{Status: StatusSucceeded, Activities: r.runs, Variables: r.variables}
	if failed != "" {
		res.Status = StatusFailed
		res.Error = failed
	}
	return res
}

// ctx builds an evaluation context over the current scope, optionally with a
// ForEach item.
func (r *run) ctx(item value, hasItem bool) *evalContext {
	return &evalContext{
		Parameters: r.params,
		Variables:  r.variables,
		Activities: r.outputs,
		Item:       item,
		HasItem:    hasItem,
	}
}

// runActivities runs a list honoring dependsOn, returning the first failure
// message (empty if all Succeeded/Skipped). `item` threads a ForEach item into
// nested scopes.
func (r *run) runActivities(acts []Activity, item value) string {
	hasItem := item != nil
	status := map[string]string{}
	var firstFailure string

	remaining := len(acts)
	for remaining > 0 {
		progressed := false
		for i := range acts {
			a := &acts[i]
			if _, done := status[a.Name]; done {
				continue
			}
			ready, satisfied := depsReady(a.DependsOn, status)
			if !ready {
				continue
			}
			progressed = true
			remaining--
			if !satisfied {
				status[a.Name] = StatusSkipped
				r.record(a, StatusSkipped, nil, "", 0)
				r.outputs[a.Name] = map[string]value{"status": StatusSkipped}
				continue
			}
			st, errMsg := r.runWithPolicy(a, item, hasItem)
			status[a.Name] = st
			if st == StatusFailed && firstFailure == "" {
				firstFailure = fmt.Sprintf("activity %q failed: %s", a.Name, errMsg)
			}
		}
		if !progressed {
			// A dependency cycle or a reference to an unknown activity.
			for i := range acts {
				a := &acts[i]
				if _, done := status[a.Name]; !done {
					status[a.Name] = StatusFailed
					r.record(a, StatusFailed, nil, "unresolvable dependency (cycle or unknown activity)", 0)
					if firstFailure == "" {
						firstFailure = fmt.Sprintf("activity %q has an unresolvable dependency", a.Name)
					}
				}
			}
			break
		}
	}
	return firstFailure
}

// depsReady reports whether every dependency has a final status (ready), and
// whether all dependency conditions are satisfied (else the activity is skipped).
func depsReady(deps []Dependency, status map[string]string) (ready, satisfied bool) {
	for _, d := range deps {
		st, ok := status[d.Activity]
		if !ok {
			return false, false
		}
		if !conditionMet(d.DependencyConditions, st) {
			satisfied = false
			// Still ready (all deps final), just not satisfied.
			ready = true
			// Keep scanning to ensure all deps are final.
			for _, d2 := range deps {
				if _, ok := status[d2.Activity]; !ok {
					return false, false
				}
			}
			return true, false
		}
	}
	return true, true
}

func conditionMet(conds []string, st string) bool {
	if len(conds) == 0 {
		conds = []string{StatusSucceeded}
	}
	for _, c := range conds {
		switch c {
		case "Succeeded":
			if st == StatusSucceeded {
				return true
			}
		case "Failed":
			if st == StatusFailed {
				return true
			}
		case "Completed":
			if st == StatusSucceeded || st == StatusFailed {
				return true
			}
		case "Skipped":
			if st == StatusSkipped {
				return true
			}
		}
	}
	return false
}

// record appends a flattened activity run.
func (r *run) record(a *Activity, status string, output map[string]any, errMsg string, dur float64) {
	r.runs = append(r.runs, ActivityRun{
		Name: a.Name, Type: a.Type, Status: status, Output: output, Error: errMsg, Duration: dur,
	})
}

// setOutput records an activity's success output for @activity(name).output.
func (r *run) setOutput(name string, output map[string]any) {
	m := map[string]value{"status": StatusSucceeded}
	if output != nil {
		m["output"] = mapToValue(output)
	}
	r.outputs[name] = m
}

func mapToValue(m map[string]any) map[string]value {
	out := make(map[string]value, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
