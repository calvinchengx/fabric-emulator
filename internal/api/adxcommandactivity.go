package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// The Azure Data Explorer Command activity — ADF's `AzureDataExplorerCommand`.
//
// ORACLE: ADF's published schema. Discriminator `AzureDataExplorerCommand`,
// required `command` ("a control command, according to the Azure Data Explorer
// command syntax"), optional `commandTimeout` in D.HH:MM:SS.
//
// THIS ONE RUNS FOR REAL, and for the plainest reason in the set: the emulator
// already hosts Microsoft's own Kusto engine behind Eventhouse (CI proves it
// with kustainer), and a control command is exactly what that engine's
// /v1/rest/mgmt endpoint takes. No cluster is proxied to; the activity's
// contract is answered here and THE REAL ENGINE COMPUTES — the Livy precedent
// again, and the same relay path the KQL data plane already uses, so the two
// cannot drift on database isolation or engine errors.
//
// It reached this file because a discriminator diff found it sitting in the
// dispatch default, reported as `Succeeded` while nothing ran. A `.drop table`
// that silently succeeded is the version of that bug worth picturing.
//
// CONNECTION STAND-IN, stated rather than implied: in ADF the cluster and
// database live on an `AzureDataExplorerLinkedService`. The emulator models no
// connections, so the target is named directly as a {workspaceId?, itemId}
// location — the same scoped mapping Script/StoredProcedure and Copy already
// use — and it must name a KQLDatabase item. Naming anything else fails BY
// NAME, because the alternative is creating an engine database from the wrong
// item's GUID and reporting success against it.
//
// A COMMAND MUST BE A CONTROL COMMAND. The schema says so, and control commands
// begin with a dot; a query sent here would be relayed to /mgmt, where it is
// not a query at all. Refusing it names the mistake instead of surfacing the
// engine's less specific complaint — the same posture as the Functions
// activity refusing a `body` on GET because the schema forbids it.
func (e *pipelineExecutor) adxCommandActivity(
	act pipeline.Activity,
	tp map[string]json.RawMessage,
	resolve func(json.RawMessage) (any, error),
) (map[string]any, error) {
	command := ""
	if raw, ok := tp["command"]; ok && len(raw) > 0 {
		v, err := resolve(raw)
		if err != nil {
			return nil, fmt.Errorf("data explorer command %q: command: %w", act.Name, err)
		}
		if v != nil {
			command = strings.TrimSpace(fmt.Sprint(v))
		}
	}
	if command == "" {
		return nil, fmt.Errorf("data explorer command %q: command is required", act.Name)
	}
	if !strings.HasPrefix(command, ".") {
		return nil, fmt.Errorf("data explorer command %q: %q is not a control command — this "+
			"activity's contract is Data Explorer CONTROL commands, which begin with a dot "+
			"(.create, .ingest, .show). A query belongs in a Lookup or a KQL query surface; "+
			"relaying it as a management command would fail at the engine for a reason that "+
			"describes the engine rather than the mistake", act.Name, snippet([]byte(command)))
	}

	timeout := time.Duration(0)
	if raw, ok := tp["commandTimeout"]; ok && len(raw) > 0 {
		v, err := resolve(raw)
		if err != nil {
			return nil, fmt.Errorf("data explorer command %q: commandTimeout: %w", act.Name, err)
		}
		if s := strings.TrimSpace(fmt.Sprint(v)); s != "" && s != "<nil>" {
			d, ok := pipeline.ParseTimeout(s)
			if !ok || d <= 0 {
				return nil, fmt.Errorf("data explorer command %q: commandTimeout %q is not "+
					"D.HH:MM:SS", act.Name, s)
			}
			timeout = d
		}
	}

	wsID, itemID, err := e.resolveDatabaseRef(tp, resolve, "database", "dataset", "linkedService")
	if err != nil {
		return nil, fmt.Errorf("data explorer command %q: %w", act.Name, err)
	}
	// One branch for both failures on purpose: whether the item is missing or
	// merely the wrong kind, the consequence is identical — proceeding would
	// create an engine database from that id and report success against a
	// target nobody meant.
	db, err := e.a.Store.GetItem(wsID, itemID)
	if err != nil || db.Type != "KQLDatabase" {
		what := "a missing item"
		if db != nil {
			what = fmt.Sprintf("%q, which is a %s", db.DisplayName, db.Type)
		}
		return nil, fmt.Errorf("data explorer command %q: the target is %s, and a control "+
			"command runs against a KQLDatabase — naming anything else would create an engine "+
			"database from its id and report success against the wrong target", act.Name, what)
	}

	if e.a.KQLURL == nil {
		return nil, fmt.Errorf("data explorer command %q: no Kusto engine is attached, so the "+
			"command was not run — start the emulator with --kql-url (FABRIC_KQL_URL) pointing "+
			"at one", act.Name)
	}

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// Same isolation and same first-use creation the data plane performs, by
	// calling the same helpers rather than a parallel copy of them.
	engineDB := engineDatabaseName(db.ID)
	if err := e.a.ensureKustoDatabase(ctx, engineDB); err != nil {
		return nil, fmt.Errorf("data explorer command %q: %v", act.Name, err)
	}
	status, payload, err := e.a.callKusto(ctx, "v1", "mgmt", kustoRequest{DB: engineDB, CSL: command})
	if err != nil {
		return nil, fmt.Errorf("data explorer command %q: %v", act.Name, err)
	}
	if status >= 300 {
		return nil, fmt.Errorf("data explorer command %q: the engine returned %d: %s",
			act.Name, status, truncate(payload))
	}

	// The engine's own v1 envelope is surfaced rather than reshaped into an
	// invented ADF output contract — no capture of the real activity's output
	// shape exists, and naming fields it may not have is how a wire claim gets
	// fabricated. Its internal database name is mapped back to the Fabric
	// display name first, exactly as the relay does, so `.show database` in a
	// pipeline reads the same as `.show database` from a client.
	var envelope map[string]any
	_ = json.Unmarshal([]byte(strings.ReplaceAll(string(payload), engineDB, db.DisplayName)), &envelope)

	return map[string]any{
		"status":   "Succeeded",
		"database": db.DisplayName,
		"tables":   envelope["Tables"],
		// Named, as everywhere else, so a run cannot be misread as a cluster.
		"executedBy": "the Kusto engine attached to the emulator, not an Azure Data Explorer cluster",
	}, nil
}
