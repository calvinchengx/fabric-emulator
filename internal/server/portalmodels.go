package server

// Semantic models, as the portal shows them.
//
// The flow view already draws a semantic model as a NODE — one box, with edges
// into it from the tables it reads. That is the right altitude for a lineage
// graph and the wrong one for the question "what is actually in this model?",
// which is the question anyone debugging a DAX result or a Direct Lake binding
// is really asking. A measure's expression, the column a relationship joins on,
// which Delta table a table is bound to: all of it is in the definition the
// emulator already parses on every executeQueries call, and none of it was
// visible anywhere.
//
// PARSED, NOT ECHOED. `getDefinition` already hands back the raw parts, so a
// view could have base64-decoded them in the browser. It would then be a second
// implementation of TMSL/TMDL reading, in a different language, free to
// disagree with the one that answers queries. This runs the SAME parser the
// evaluator uses (internal/semanticmodel), so what the portal shows is what the
// emulator believes — including, usefully, when that is nothing: a model whose
// definition does not parse shows its error instead of vanishing.

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// errNoModelPart: the item has a definition, but nothing in it is a model.
var errNoModelPart = errors.New("no model.bim and no .tmdl parts in definition")

type portalColumn struct {
	Name         string `json:"name"`
	DataType     string `json:"dataType"`
	SourceColumn string `json:"sourceColumn,omitempty"`
}

type portalMeasure struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
}

type portalModelTable struct {
	Name     string          `json:"name"`
	Columns  []portalColumn  `json:"columns"`
	Measures []portalMeasure `json:"measures"`
	// Binding is where the table's rows come from, in one readable string:
	// a Direct Lake entity, or "" for an import table. The distinction is the
	// single most consequential thing about a Fabric table and is invisible
	// from a column list.
	Binding string `json:"binding,omitempty"`
	Mode    string `json:"mode"`
}

type portalRelationship struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}

type portalModel struct {
	ItemID      string               `json:"itemId"`
	WorkspaceID string               `json:"workspaceId"`
	Workspace   string               `json:"workspace"`
	DisplayName string               `json:"displayName"`
	ModelName   string               `json:"modelName,omitempty"`
	Compat      int                  `json:"compatibilityLevel,omitempty"`
	Format      string               `json:"format,omitempty"` // TMSL | TMDL
	Tables      []portalModelTable   `json:"tables"`
	Rels        []portalRelationship `json:"relationships"`
	// RowsLoaded reports whether an inline data.json snapshot is present, which
	// is what makes a query answerable at all on an import model. A model with
	// tables and no rows answers every DAX query with nothing, and that looks
	// identical to a wrong measure until you know.
	RowsLoaded bool `json:"rowsLoaded"`
	// Error is the parse failure, when there is one. Reported rather than
	// dropped: a model missing from a list reads as "not published", which is a
	// different problem from "published and unreadable".
	Error string `json:"error,omitempty"`
}

func (s *Server) portalModels(w http.ResponseWriter, r *http.Request) {
	workspaces, err := s.Store.ListAllWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	out := []portalModel{}
	for _, ws := range workspaces {
		items, err := s.Store.ListItems(ws.ID, "SemanticModel")
		if err != nil {
			continue
		}
		for _, it := range items {
			out = append(out, s.describeModel(ws, it))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": out})
}

func (s *Server) describeModel(ws *store.Workspace, it *store.Item) portalModel {
	pm := portalModel{
		ItemID: it.ID, WorkspaceID: ws.ID, Workspace: ws.DisplayName,
		DisplayName: it.DisplayName, Tables: []portalModelTable{}, Rels: []portalRelationship{},
	}
	parts, err := s.Store.GetDefinition(it.ID)
	if err != nil || len(parts) == 0 {
		pm.Error = "no definition"
		return pm
	}

	m, format, err := parseModelParts(parts)
	if err != nil {
		pm.Error = err.Error()
		return pm
	}
	pm.Format = format
	pm.ModelName = m.Name
	pm.Compat = m.CompatibilityLevel

	for _, t := range m.Tables {
		row := portalModelTable{
			Name: t.Name, Mode: "import",
			Columns: []portalColumn{}, Measures: []portalMeasure{},
		}
		if t.DirectLake != nil {
			row.Mode = "directLake"
			// schema.entity when a schema is named, entity alone otherwise —
			// the same shape a Fabric user sees on the binding itself.
			row.Binding = t.DirectLake.EntityName
			if t.DirectLake.SchemaName != "" {
				row.Binding = t.DirectLake.SchemaName + "." + t.DirectLake.EntityName
			}
		}
		for _, c := range t.Columns {
			row.Columns = append(row.Columns, portalColumn{
				Name: c.Name, DataType: c.DataType, SourceColumn: c.SourceColumn,
			})
		}
		for _, ms := range t.Measures {
			row.Measures = append(row.Measures, portalMeasure{Name: ms.Name, Expression: ms.Expression})
		}
		pm.Tables = append(pm.Tables, row)
	}
	for _, rel := range m.Relationships {
		pm.Rels = append(pm.Rels, portalRelationship{
			Name: rel.Name,
			From: rel.FromTable + "[" + rel.FromColumn + "]",
			To:   rel.ToTable + "[" + rel.ToColumn + "]",
		})
	}
	for _, p := range parts {
		if p.Path == "data.json" {
			pm.RowsLoaded = true
		}
	}
	return pm
}

// parseModelParts reads whichever serialisation the definition carries, in the
// same precedence executeQueries uses: TMSL first, then TMDL.
//
// Mirroring that order matters more than it looks. A definition carrying both
// is answered by model.bim at query time, so a portal that preferred the TMDL
// would describe a model nobody is querying — and would do it convincingly.
func parseModelParts(parts []store.DefinitionPart) (*semanticmodel.Model, string, error) {
	tmdl := map[string][]byte{}
	for _, p := range parts {
		raw, err := base64.StdEncoding.DecodeString(p.Payload)
		if err != nil {
			continue
		}
		if p.Path == "model.bim" {
			m, err := semanticmodel.ParseTMSL(raw)
			if err != nil {
				return nil, "", err
			}
			return m, "TMSL", nil
		}
		if strings.HasSuffix(strings.ToLower(p.Path), ".tmdl") {
			tmdl[p.Path] = raw
		}
	}
	if len(tmdl) > 0 {
		m, err := semanticmodel.ParseTMDL(tmdl)
		if err != nil {
			return nil, "", err
		}
		return m, "TMDL", nil
	}
	return nil, "", errNoModelPart
}
