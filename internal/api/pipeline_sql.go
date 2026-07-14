package api

// Real T-SQL leaf activities for the pipeline interpreter: Script and
// SqlServerStoredProcedure run against a Warehouse or Fabric SQL Database
// item's own SQL Server database (the same per-item backend the TDS endpoint
// and the SQLDatabase mirror use) — real queries, real DDL/DML, real rows back.
// Real Fabric addresses these through a linkedService/connection reference;
// the emulator's scoped mapping instead names the target directly as a
// {workspaceId?, itemId} location (the same shape Copy/Lookup/GetMetadata
// already use), consistent within the family rather than byte-identical to
// the wire shape — the same stance Copy takes for its connector set.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// resolveDatabaseRef reads a Script/StoredProcedure activity's database
// reference — {workspaceId?, itemId}, optionally nested under "location" (like
// Copy/Lookup) — and resolves it to a Warehouse/SQLDatabase item id. Unlike
// resolveLoc, no "path" is required (a database reference, not a file).
func (e *pipelineExecutor) resolveDatabaseRef(tp map[string]json.RawMessage, resolve func(json.RawMessage) (any, error), keys ...string) (itemID string, err error) {
	for _, k := range keys {
		raw, ok := tp[k]
		if !ok {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			continue
		}
		loc := obj
		if l, ok := obj["location"]; ok {
			loc = map[string]json.RawMessage{}
			_ = json.Unmarshal(l, &loc)
		}
		field := func(k string) (string, error) {
			raw, ok := loc[k]
			if !ok {
				return "", nil
			}
			v, err := resolve(raw)
			if err != nil {
				return "", err
			}
			if v == nil {
				return "", nil
			}
			return fmt.Sprint(v), nil
		}
		wsRef, err := field("workspaceId")
		if err != nil {
			return "", err
		}
		itemRef, err := field("itemId")
		if err != nil {
			return "", err
		}
		if itemRef == "" {
			continue
		}
		_, id, err := e.resolveItemRef(wsRef, itemRef)
		return id, err
	}
	return "", fmt.Errorf("no database reference (%s)", strings.Join(keys, "/"))
}

// sqlDB resolves the activity's target database and hands back its connection,
// erroring loudly (not silently) when no SQL backend is attached.
func (e *pipelineExecutor) sqlDB(tp map[string]json.RawMessage, resolve func(json.RawMessage) (any, error)) (*sql.DB, error) {
	itemID, err := e.resolveDatabaseRef(tp, resolve, "database", "dataset", "linkedService")
	if err != nil {
		return nil, err
	}
	if e.a.SQLDB == nil {
		return nil, fmt.Errorf("no warehouse SQL backend attached (set --warehouse-sql-url)")
	}
	return e.a.SQLDB(context.Background(), itemID)
}

// scriptActivity runs each of typeProperties.scripts against the target
// database: a "Query" script's rows come back per-script; a "NonQuery" script
// reports rows affected. Matches the well-known ADF/Fabric Script activity
// scripts[] shape.
func (e *pipelineExecutor) scriptActivity(act pipeline.Activity, tp map[string]json.RawMessage, resolve func(json.RawMessage) (any, error)) (map[string]any, error) {
	db, err := e.sqlDB(tp, resolve)
	if err != nil {
		return nil, fmt.Errorf("script %q: %w", act.Name, err)
	}
	var scripts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(tp["scripts"], &scripts); err != nil || len(scripts) == 0 {
		return nil, fmt.Errorf("script %q: typeProperties.scripts is required", act.Name)
	}

	ctx := context.Background()
	resultSets := make([]map[string]any, 0, len(scripts))
	var totalAffected int64
	for i, s := range scripts {
		if strings.EqualFold(s.Type, "NonQuery") {
			res, err := db.ExecContext(ctx, s.Text)
			if err != nil {
				return nil, fmt.Errorf("script %q: script %d (NonQuery): %w", act.Name, i, err)
			}
			n, _ := res.RowsAffected()
			totalAffected += n
			resultSets = append(resultSets, map[string]any{"rowCount": n})
			continue
		}
		rows, err := db.QueryContext(ctx, s.Text)
		if err != nil {
			return nil, fmt.Errorf("script %q: script %d (Query): %w", act.Name, i, err)
		}
		out, err := scanRows(rows)
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("script %q: script %d (Query): %w", act.Name, i, err)
		}
		resultSets = append(resultSets, map[string]any{"rowCount": len(out), "rows": out})
	}
	return map[string]any{"resultSets": resultSets, "recordsAffected": totalAffected}, nil
}

// storedProcedureActivity calls typeProperties.storedProcedureName with named
// parameters (storedProcedureParameters: {name: {value, type?}}), matching the
// ADF/Fabric Stored Procedure activity shape.
func (e *pipelineExecutor) storedProcedureActivity(act pipeline.Activity, tp map[string]json.RawMessage, resolve func(json.RawMessage) (any, error)) (map[string]any, error) {
	db, err := e.sqlDB(tp, resolve)
	if err != nil {
		return nil, fmt.Errorf("stored procedure %q: %w", act.Name, err)
	}
	var procName string
	if err := json.Unmarshal(tp["storedProcedureName"], &procName); err != nil || procName == "" {
		return nil, fmt.Errorf("stored procedure %q: typeProperties.storedProcedureName is required", act.Name)
	}
	var params map[string]struct {
		Value any `json:"value"`
	}
	_ = json.Unmarshal(tp["storedProcedureParameters"], &params)

	var placeholders []string
	args := make([]any, 0, len(params))
	for name, p := range params {
		placeholders = append(placeholders, fmt.Sprintf("@%s = @p%d", name, len(args)+1))
		args = append(args, sql.Named(fmt.Sprintf("p%d", len(args)+1), p.Value))
	}
	query := "EXEC " + procName
	if len(placeholders) > 0 {
		query += " " + strings.Join(placeholders, ", ")
	}

	ctx := context.Background()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("stored procedure %q: %w", act.Name, err)
	}
	defer rows.Close()
	out, err := scanRows(rows)
	if err != nil {
		return nil, fmt.Errorf("stored procedure %q: %w", act.Name, err)
	}
	return map[string]any{"resultSets": []map[string]any{{"rowCount": len(out), "rows": out}}}, nil
}

// scanRows materializes a *sql.Rows into column-keyed row maps ([]byte
// normalized to string, so JSON output is text rather than base64).
func scanRows(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			v := vals[i]
			if bs, ok := v.([]byte); ok {
				v = string(bs)
			}
			m[c] = v
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
