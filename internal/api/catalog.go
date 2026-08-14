package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
)

// Catalog Search (POST /v1/catalog/search) — preview REST that the Core MCP
// Server's search_catalog tool calls. Cross-workspace metadata over items the
// caller can already see; it does not grant data-plane access.
//
// Reference: rest/api/fabric/core/catalog/search. Dashboard and Dataflow
// (Gen1/Gen2) are excluded as that page documents.

func (a *API) registerCatalog(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/catalog/search", a.withAuth(a.searchCatalog))
}

type catalogQueryRequest struct {
	Search            string `json:"search"`
	Filter            string `json:"filter"`
	PageSize          int    `json:"pageSize"`
	ContinuationToken string `json:"continuationToken"`
}

type catalogEntry struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	CatalogEntryType string `json:"catalogEntryType"`
	DisplayName      string `json:"displayName"`
	Description      string `json:"description"`
	Hierarchy        struct {
		Workspace struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"workspace"`
	} `json:"hierarchy"`
}

func (a *API) searchCatalog(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req catalogQueryRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "InvalidRequest", "Malformed JSON body.")
			return
		}
	}
	if req.PageSize == 0 {
		req.PageSize = 50
	}
	if req.PageSize < 1 || req.PageSize > 1000 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "pageSize must be between 1 and 1000.")
		return
	}

	workspaces, err := a.Store.ListWorkspacesFor(p.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	q := strings.ToLower(strings.TrimSpace(req.Search))
	var entries []catalogEntry
	for _, ws := range workspaces {
		items, err := a.Store.ListItems(ws.ID, "")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
		for _, it := range items {
			if catalogUnsupported(it.Type) {
				continue
			}
			ok, err := typeFilterAllows(req.Filter, it.Type)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "InvalidRequest", err.Error())
				return
			}
			if !ok {
				continue
			}
			if q != "" &&
				!strings.Contains(strings.ToLower(it.DisplayName), q) &&
				!strings.Contains(strings.ToLower(it.Description), q) &&
				!strings.Contains(strings.ToLower(ws.DisplayName), q) {
				continue
			}
			e := catalogEntry{
				ID:               it.ID,
				Type:             it.Type,
				CatalogEntryType: "FabricItem",
				DisplayName:      it.DisplayName,
				Description:      it.Description,
			}
			e.Hierarchy.Workspace.ID = ws.ID
			e.Hierarchy.Workspace.DisplayName = ws.DisplayName
			entries = append(entries, e)
		}
	}
	if entries == nil {
		entries = []catalogEntry{}
	}

	offset := 0
	if req.ContinuationToken != "" {
		if n := decodePageToken(req.ContinuationToken); n > 0 {
			offset = n
		}
	}
	if offset > len(entries) {
		offset = len(entries)
	}
	end := offset + req.PageSize
	if end > len(entries) {
		end = len(entries)
	}
	page := entries[offset:end]
	resp := map[string]any{"value": page}
	if end < len(entries) {
		resp["continuationToken"] = encodePageToken(end)
	}
	writeJSON(w, http.StatusOK, resp)
}

func catalogUnsupported(itemType string) bool {
	switch strings.ToLower(itemType) {
	case "dashboard", "dataflow":
		return true
	default:
		return false
	}
}

func typeFilterAllows(filter, itemType string) (bool, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true, nil
	}
	p := &filterParser{s: filter}
	ok, err := p.orExpr(itemType)
	if err != nil {
		return false, err
	}
	p.skip()
	if p.i < len(p.s) {
		return false, errors.New("invalid filter")
	}
	return ok, nil
}

type filterParser struct {
	s string
	i int
}

func (p *filterParser) skip() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

func (p *filterParser) orExpr(typ string) (bool, error) {
	v, err := p.andExpr(typ)
	if err != nil {
		return false, err
	}
	for {
		p.skip()
		if !p.keyword("or") {
			return v, nil
		}
		rhs, err := p.andExpr(typ)
		if err != nil {
			return false, err
		}
		v = v || rhs
	}
}

func (p *filterParser) andExpr(typ string) (bool, error) {
	v, err := p.unary(typ)
	if err != nil {
		return false, err
	}
	for {
		p.skip()
		if !p.keyword("and") {
			return v, nil
		}
		rhs, err := p.unary(typ)
		if err != nil {
			return false, err
		}
		v = v && rhs
	}
}

func (p *filterParser) unary(typ string) (bool, error) {
	p.skip()
	if p.i < len(p.s) && p.s[p.i] == '(' {
		p.i++
		v, err := p.orExpr(typ)
		if err != nil {
			return false, err
		}
		p.skip()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return false, errors.New("invalid filter: missing )")
		}
		p.i++
		return v, nil
	}
	if !p.keyword("type") {
		return false, errors.New("invalid filter: expected Type")
	}
	p.skip()
	eq := p.keyword("eq")
	ne := !eq && p.keyword("ne")
	if !eq && !ne {
		return false, errors.New("invalid filter: expected eq or ne")
	}
	p.skip()
	lit, err := p.quoted()
	if err != nil {
		return false, err
	}
	match := strings.EqualFold(typ, lit)
	if ne {
		return !match, nil
	}
	return match, nil
}

func (p *filterParser) keyword(k string) bool {
	p.skip()
	if p.i+len(k) > len(p.s) {
		return false
	}
	got := p.s[p.i : p.i+len(k)]
	if !strings.EqualFold(got, k) {
		return false
	}
	end := p.i + len(k)
	if end < len(p.s) {
		c := p.s[end]
		if c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			return false
		}
	}
	p.i = end
	return true
}

func (p *filterParser) quoted() (string, error) {
	p.skip()
	if p.i >= len(p.s) || (p.s[p.i] != '\'' && p.s[p.i] != '"') {
		return "", errors.New("invalid filter: expected quoted type")
	}
	q := p.s[p.i]
	p.i++
	start := p.i
	for p.i < len(p.s) && p.s[p.i] != q {
		p.i++
	}
	if p.i >= len(p.s) {
		return "", errors.New("invalid filter: unterminated string")
	}
	s := p.s[start:p.i]
	p.i++
	return s, nil
}
