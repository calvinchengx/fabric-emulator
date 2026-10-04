package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/calvinchengx/fabric-emulator/internal/api"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tds"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// versionedRoute gives a Warehouse connection the time travel its version
// history supports (docs/35 Phase 4). A SQL Database never has one.
func versionedRoute(route sqlRoute, st *store.Store, retentionDays int) sqlRoute {
	return func(ctx context.Context, server, database, principal string) (tds.Connection, error) {
		conn, err := route(ctx, server, database, principal)
		if err != nil {
			return conn, err
		}
		if it, gerr := st.GetItemByID(conn.TargetDB); gerr == nil && it.Type == "Warehouse" {
			conn.TimeTravel = warehouse.WarehouseTimeTravelResolver(st, it.ID, retentionDays)
		}
		return conn, nil
	}
}

// sqlExecTimeout bounds one batch. Microsoft's skills-for-fabric records 300
// seconds as the observed timeout of Fabric's Data Warehouse MCP server, and
// says it is not a documented contract.
const sqlExecTimeout = 300 * time.Second

// sqlExecAsFor builds the hook Fabric's Data Warehouse MCP server runs T-SQL
// through: route decides the caller's connection exactly as it does for a TDS
// login, the batch is refused or adapted by the wire's own rules
// (tds.PrepareBatch), it runs logged in AS the caller so the engine enforces
// grants and row-, column- and object-level security, and a write it accepts is
// observed for lineage and versioning as a relayed one is. The TDS server is
// read at call time for Strict and Observe, which server.New sets after this.
func sqlExecAsFor(be principalBackend, route sqlRoute, wire *tds.Server) func(ctx context.Context, itemID, principal, query string, maxRows int) (*api.SQLBatchResult, error) {
	return func(ctx context.Context, itemID, principal, query string, maxRows int) (*api.SQLBatchResult, error) {
		conn, err := route(ctx, "", itemID, principal)
		if err != nil {
			return nil, err
		}
		stmt, refusal := tds.PrepareBatch(conn, query, wire.Strict)
		if refusal != "" {
			return nil, errors.New(refusal)
		}
		db, err := be.DBAs(ctx, conn.TargetDB, principal, tds.TargetFirst(conn))
		if err != nil {
			return nil, err
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(ctx, sqlExecTimeout)
		defer cancel()
		res, err := lastResultSet(ctx, db, stmt, maxRows)
		if err != nil {
			return nil, engineMessage(err)
		}
		tds.ObserveBatch(wire.Observe, conn.TargetDB, stmt)
		return res, nil
	}
}

// engineMessage is SQL Server's own words for err, without the driver's
// "mssql: " prefix and "(number)" suffix: Fabric's server was captured
// answering "Invalid object name 'dbo.NoSuchTable'." and nothing more.
func engineMessage(err error) error {
	var e mssql.Error
	if errors.As(err, &e) {
		return errors.New(e.Message)
	}
	return err
}

// lastResultSet runs one batch and keeps its last result set — the one
// Fabric's Data Warehouse MCP server returns — reading at most maxRows rows of
// it. A batch with no result set at all (DDL, DML) returns no columns.
func lastResultSet(ctx context.Context, db *sql.DB, stmt string, maxRows int) (*api.SQLBatchResult, error) {
	rows, err := db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	last := &api.SQLBatchResult{}
	for {
		cols, err := rows.ColumnTypes()
		if err != nil {
			return nil, err
		}
		if len(cols) > 0 {
			set := &api.SQLBatchResult{}
			for _, c := range cols {
				set.Columns = append(set.Columns, c.Name())
				set.Types = append(set.Types, c.DatabaseTypeName())
			}
			for rows.Next() {
				if len(set.Rows) == maxRows {
					set.Truncated = true
					continue // drain, so the batch's later sets are reached
				}
				row, err := scanRow(rows, set.Types)
				if err != nil {
					return nil, err
				}
				set.Rows = append(set.Rows, row)
			}
			last = set
		}
		if !rows.NextResultSet() {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return last, nil
}

// scanRow reads one row as values a CSV writer can print: text stays text, a
// uniqueidentifier is decoded from SQL Server's mixed-endian bytes, and other
// binary is hex, as SQL Server's own tools show it.
func scanRow(rows *sql.Rows, types []string) ([]any, error) {
	vals := make([]any, len(types))
	ptrs := make([]any, len(types))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	for i, v := range vals {
		bs, ok := v.([]byte)
		if !ok {
			continue
		}
		switch strings.ToUpper(types[i]) {
		case "UNIQUEIDENTIFIER":
			var id mssql.UniqueIdentifier
			if err := id.Scan(bs); err != nil {
				return nil, fmt.Errorf("column %d: %w", i, err)
			}
			vals[i] = id.String()
		case "BINARY", "VARBINARY", "IMAGE", "TIMESTAMP", "ROWVERSION":
			vals[i] = fmt.Sprintf("0x%X", bs)
		default:
			vals[i] = string(bs)
		}
	}
	return vals, nil
}
