package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// The Validation activity — ADF's `Validation`, which blocks a pipeline until
// the data it is about to process is actually there.
//
// ORACLE: ADF's published schema. Discriminator `Validation`, required
// `dataset`; `timeout` defaulting to "TimeSpan.FromDays(7) which is 1 week";
// `sleep`, "a delay in seconds between validation attempts", default 10;
// `minimumSize`, usable "if dataset points to a file", the file "must be
// greater than or equal in size"; `childItems`, usable "if dataset points to a
// folder" — true means "the folder must have at least one file", false means
// "the folder must be empty".
//
// THIS ONE RUNS FOR REAL because everything it needs is already here: the
// paths are OneLake paths the emulator serves, the sizes are real bytes, and
// the waiting is the virtual clock's job. It was found in the dispatch default
// reporting `Succeeded` — which for THIS activity is a particularly bad
// failure, since its entire purpose is to stop a pipeline from processing data
// that has not landed. A Validation that always passes is worse than no
// Validation: the pipeline reads an absent file with the guard's blessing.
//
// CONNECTION STAND-IN, as elsewhere: ADF names the target with a dataset
// reference, and the emulator models no datasets, so the location is given
// directly as {workspaceId?, itemId, path} — the shape Copy, Lookup and
// GetMetadata already take, resolved by the same readLoc.
//
// WAITING IS MEASURED ON THE VIRTUAL CLOCK, not with real sleeps: a frozen
// clock advanced past the deadline MUST expire the wait, and `sleep` between
// attempts is virtual too. The loop below subscribes to the clock's change
// broadcast BEFORE reading it — the ordering that a scripted clock pins in the
// tests, and the same discipline the WebHook park had to learn.

const (
	validationDefaultTimeout = 7 * 24 * time.Hour
	validationDefaultSleep   = 10
)

// validationWait polls `check` until it passes or the virtual deadline passes,
// sleeping `sleep` virtual seconds between attempts. It returns the last
// failure reason so the timeout error can say what was still wrong, rather than
// only that time ran out. Extracted from the activity so its interleaving can
// be driven deterministically: the subscribe-before-read window is too small
// to sample for.
func validationWait(clk clockSource, deadline, sleep int64, check func() (bool, string)) (bool, string) {
	for {
		// Subscribe first, then look. Reversed, a clock change landing between
		// the check and the select closes a channel nobody is holding, and the
		// wait sleeps through its own deadline.
		clockChanged := clk.Changed()
		ok, why := check()
		if ok {
			return true, ""
		}
		remaining := deadline - clk.Now()
		if remaining <= 0 {
			return false, why
		}
		wait := sleep
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-clockChanged:
		case <-time.After(time.Duration(wait) * time.Second):
		}
	}
}

func (e *pipelineExecutor) validationActivity(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	loc, err := e.readLoc(tp, resolve, "dataset", "source", "location")
	if err != nil {
		return nil, fmt.Errorf("validation %q: %w", act.Name, err)
	}

	timeout := validationDefaultTimeout
	if raw, ok := tp["timeout"]; ok && len(raw) > 0 {
		v, rerr := resolve(raw)
		if rerr != nil {
			return nil, fmt.Errorf("validation %q: timeout: %w", act.Name, rerr)
		}
		if s := strings.TrimSpace(fmt.Sprint(v)); s != "" && s != "<nil>" {
			d, ok := pipeline.ParseTimeout(s)
			if !ok || d <= 0 {
				return nil, fmt.Errorf("validation %q: timeout %q is not D.HH:MM:SS", act.Name, s)
			}
			timeout = d
		}
	}

	sleep := int64(validationDefaultSleep)
	if raw, ok := tp["sleep"]; ok && len(raw) > 0 {
		v, rerr := resolve(raw)
		if rerr != nil {
			return nil, fmt.Errorf("validation %q: sleep: %w", act.Name, rerr)
		}
		n, ok := asInt(v)
		if !ok || n <= 0 {
			return nil, fmt.Errorf("validation %q: sleep must be a positive number of seconds, "+
				"got %v", act.Name, v)
		}
		sleep = int64(n)
	}

	minimumSize := -1
	if raw, ok := tp["minimumSize"]; ok && len(raw) > 0 {
		v, rerr := resolve(raw)
		if rerr != nil {
			return nil, fmt.Errorf("validation %q: minimumSize: %w", act.Name, rerr)
		}
		n, ok := asInt(v)
		if !ok || n < 0 {
			return nil, fmt.Errorf("validation %q: minimumSize must be a non-negative number of "+
				"bytes, got %v", act.Name, v)
		}
		minimumSize = n
	}

	// Tri-state on purpose: absent is not the same as false. `childItems:false`
	// asserts the folder is EMPTY, which is a real assertion a pipeline can
	// depend on, and collapsing it into "unset" would pass on a folder full of
	// files.
	var childItems *bool
	if raw, ok := tp["childItems"]; ok && len(raw) > 0 {
		v, rerr := resolve(raw)
		if rerr != nil {
			return nil, fmt.Errorf("validation %q: childItems: %w", act.Name, rerr)
		}
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("validation %q: childItems must be true or false, got %v",
				act.Name, v)
		}
		childItems = &b
	}

	// The predicate, re-evaluated on each attempt because the point of the
	// activity is that the answer changes while it waits.
	check := func() (bool, string) {
		p, perr := e.a.Store.GetOneLakePath(loc.itemID, loc.path)
		if perr == nil && !p.IsDir {
			if childItems != nil {
				return false, fmt.Sprintf("%q is a file, and childItems asks about a folder",
					loc.path)
			}
			if minimumSize >= 0 && len(p.Content) < minimumSize {
				return false, fmt.Sprintf("%q is %d bytes, below the minimumSize of %d",
					loc.path, len(p.Content), minimumSize)
			}
			return true, ""
		}

		// Either an explicit directory row or nothing at all — and in OneLake
		// those are not distinguishable by a lookup, because DIRECTORIES ARE
		// IMPLICIT: writing Files/in/day.csv creates no row for Files/in. A
		// folder therefore exists exactly when something is under it, which is
		// what a listing answers and what a pipeline author means by "wait for
		// the landing folder".
		children, lerr := e.a.Store.ListOneLakePaths(loc.itemID, loc.path, false)
		if lerr != nil {
			return false, fmt.Sprintf("%q could not be listed: %v", loc.path, lerr)
		}
		if perr != nil && len(children) == 0 {
			return false, fmt.Sprintf("%q does not exist", loc.path)
		}
		if minimumSize >= 0 {
			return false, fmt.Sprintf("%q is a folder, and minimumSize asks about a file", loc.path)
		}
		if childItems == nil {
			return true, ""
		}
		files := 0
		for _, c := range children {
			if c.RelPath != loc.path && !c.IsDir {
				files++
			}
		}
		if *childItems && files == 0 {
			return false, fmt.Sprintf("%q holds no files yet", loc.path)
		}
		if !*childItems && files > 0 {
			return false, fmt.Sprintf("%q still holds %d file(s), and childItems:false asks for "+
				"an empty folder", loc.path, files)
		}
		return true, ""
	}

	deadline := e.a.Store.Now() + int64(timeout/time.Second)
	ok, why := validationWait(e.a.Store.Clock, deadline, sleep, check)
	if !ok {
		return nil, fmt.Errorf("validation %q: the data never became valid within the timeout — "+
			"%s. The activity failed rather than passing the pipeline on to read data that is "+
			"not there, which is the whole reason it is in the definition", act.Name, why)
	}

	// The output mirrors GetMetadata's vocabulary, since a validated path is a
	// path a downstream step will immediately want the facts about.
	out := map[string]any{"exists": true, "itemName": baseName(loc.path)}
	if p, perr := e.a.Store.GetOneLakePath(loc.itemID, loc.path); perr == nil {
		out["itemType"] = itemType(p.IsDir)
		if !p.IsDir {
			out["size"] = len(p.Content)
		}
	} else {
		// No row, but the check passed — an implicit directory, which is the
		// only thing it could have been.
		out["itemType"] = itemType(true)
	}
	return out, nil
}
