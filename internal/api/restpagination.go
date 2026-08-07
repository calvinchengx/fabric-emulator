package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Pagination for the REST connector (R2 of docs/40-rest-connector-plan.md).
//
// A REST API that splits a large result across pages is the normal case, not an
// edge one — Microsoft's own first pagination example in the connector
// reference is a ServiceNow table paged by `sysparm_offset`, and BMC Helix's
// `limit`/`offset` is structurally identical. R1 refused `paginationRules`
// rather than read page one of fifty and report success; this is the loop that
// replaces the refusal.
//
// Fabric composes the NEXT request from the CURRENT response. Two families
// cover everything it documents:
//
//   - a cursor: the next URL, query parameter or header is a value read out of
//     the response body (a JSONPath) or its headers — `"AbsoluteUrl": "$.paging.next"`
//   - a range: a placeholder in the request is stepped over a numeric range —
//     `"QueryParameters.{offset}": "RANGE:0::1000"`
//
// Termination is deliberately over-determined, because an endpoint whose `next`
// points at itself is a documented real case: any JSONPath resolving to null
// ends it, so does HTTP 204, so does an `EndCondition`, so does
// `MaxRequestNumber` — and failing all of those, a hard page ceiling that
// refuses rather than loops forever.

// restMaxPages backstops a `next` that never terminates. Microsoft documents
// that exact endless-loop case, and MaxRequestNumber is optional, so something
// has to be the floor.
const restMaxPages = 1000

// restPagination is the parsed `paginationRules` dictionary.
type restPagination struct {
	// absoluteURL is the value expression yielding the next request's whole URL.
	absoluteURL string
	// queryParams / headers set one query parameter or header on the next
	// request from a value expression.
	queryParams map[string]string
	headers     map[string]string
	// ranges step a `{placeholder}` in the URL or in a header.
	ranges []restRange
	// endConds terminate the loop on a shape of the response.
	endConds []restEndCond
	// maxRequests caps the page count; 0 means "no rule, use the ceiling".
	maxRequests int
	// rfc5988 follows `Link: <…>; rel="next"`. Fabric defaults this ON when no
	// other rule is defined, which is surprising enough to be worth a test.
	// rfc5988Explicit records that the author WROTE SupportRFC5988, which is what
	// separates "on by default" from "asked for": the default yields to any other
	// rule set, an explicit true does not.
	rfc5988         bool
	rfc5988Explicit bool
	// declared reports whether the author wrote any rules at all.
	declared bool
}

type restRange struct {
	where       string // AbsoluteUrl | QueryParameters | Headers
	placeholder string // the token between the braces, e.g. offset
	cur, end    float64
	step        float64
	hasEnd      bool
}

type restEndCond struct {
	expr string // "$.data" (body) or "headers.complete" (response header)
	cond string // Empty | NonExist | Exist | Const:<value>
}

// parsePaginationRules reads the dictionary, refusing any key or value form it
// does not implement BY NAME. A rule that is silently ignored is a copy that
// reads the wrong number of pages and says nothing.
func parsePaginationRules(actName string, rules map[string]string) (*restPagination, error) {
	p := &restPagination{
		queryParams: map[string]string{},
		headers:     map[string]string{},
		rfc5988:     true, // Fabric's default when nothing else is declared
	}
	if len(rules) == 0 {
		return p, nil
	}
	p.declared = true

	for key, val := range rules {
		switch {
		case key == "MaxRequestNumber":
			n, err := strconv.Atoi(strings.TrimSpace(val))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("copy %q: MaxRequestNumber %q is not a positive number",
					actName, val)
			}
			p.maxRequests = n
			continue
		case key == "SupportRFC5988":
			p.rfc5988 = !strings.EqualFold(strings.TrimSpace(val), "false")
			p.rfc5988Explicit = true
			continue
		case strings.HasPrefix(key, "EndCondition:"):
			expr := strings.TrimSpace(strings.TrimPrefix(key, "EndCondition:"))
			if expr == "" {
				return nil, fmt.Errorf("copy %q: EndCondition: needs an expression after the colon",
					actName)
			}
			p.endConds = append(p.endConds, restEndCond{expr: expr, cond: strings.TrimSpace(val)})
			continue
		}

		where, sel, err := splitRuleKey(actName, key)
		if err != nil {
			return nil, err
		}
		// A `{placeholder}` selector means the value is a RANGE stepped into the
		// request; a bare name means the value is read out of the response.
		if ph, isPlaceholder := bracedName(sel); isPlaceholder {
			r, err := parseRange(actName, key, val)
			if err != nil {
				return nil, err
			}
			r.where, r.placeholder = where, ph
			p.ranges = append(p.ranges, r)
			continue
		}
		if strings.HasPrefix(strings.ToUpper(val), "RANGE:") {
			return nil, fmt.Errorf("copy %q: %q takes a RANGE, but its key names a literal %q — "+
				"a range steps a {placeholder} that appears in the request", actName, key, sel)
		}
		switch where {
		case "AbsoluteUrl":
			p.absoluteURL = val
		case "QueryParameters":
			p.queryParams[sel] = val
		case "Headers":
			p.headers[sel] = val
		}
	}
	return p, nil
}

// splitRuleKey splits `AbsoluteUrl`, `QueryParameters.x`, `QueryParameters['x']`
// and `Headers.x` into the family and its selector.
func splitRuleKey(actName, key string) (where, sel string, err error) {
	for _, fam := range []string{"AbsoluteUrl", "QueryParameters", "Headers"} {
		if key == fam {
			return fam, "", nil
		}
		if !strings.HasPrefix(key, fam+".") && !strings.HasPrefix(key, fam+"[") {
			continue
		}
		rest := key[len(fam):]
		switch {
		case strings.HasPrefix(rest, "."):
			return fam, rest[1:], nil
		case strings.HasPrefix(rest, "['") && strings.HasSuffix(rest, "']"):
			return fam, rest[2 : len(rest)-2], nil
		case strings.HasPrefix(rest, `["`) && strings.HasSuffix(rest, `"]`):
			return fam, rest[2 : len(rest)-2], nil
		}
	}
	return "", "", fmt.Errorf("copy %q: pagination rule %q is not one the emulator implements "+
		"(AbsoluteUrl, QueryParameters.x, Headers.x, EndCondition:x, MaxRequestNumber, "+
		"SupportRFC5988)", actName, key)
}

// bracedName reports whether a selector is a `{placeholder}` and returns its name.
func bracedName(sel string) (string, bool) {
	if len(sel) > 2 && strings.HasPrefix(sel, "{") && strings.HasSuffix(sel, "}") {
		return sel[1 : len(sel)-1], true
	}
	return "", false
}

// parseRange reads `RANGE:start:end:step`. An empty end is open-ended, which is
// the documented form for "page until something else stops us".
func parseRange(actName, key, val string) (restRange, error) {
	if !strings.HasPrefix(strings.ToUpper(val), "RANGE:") {
		return restRange{}, fmt.Errorf("copy %q: pagination rule %q selects a {placeholder}, so its "+
			"value must be RANGE:start:end:step, got %q", actName, key, val)
	}
	parts := strings.Split(val[len("RANGE:"):], ":")
	if len(parts) != 3 {
		return restRange{}, fmt.Errorf("copy %q: %q: RANGE takes start:end:step (end may be empty), got %q",
			actName, key, val)
	}
	num := func(s string) (float64, error) { return strconv.ParseFloat(strings.TrimSpace(s), 64) }
	start, err := num(parts[0])
	if err != nil {
		return restRange{}, fmt.Errorf("copy %q: %q: RANGE start %q is not a number", actName, key, parts[0])
	}
	step, err := num(parts[2])
	if err != nil || step == 0 {
		return restRange{}, fmt.Errorf("copy %q: %q: RANGE step %q must be a non-zero number",
			actName, key, parts[2])
	}
	r := restRange{cur: start, step: step}
	if strings.TrimSpace(parts[1]) != "" {
		end, err := num(parts[1])
		if err != nil {
			return restRange{}, fmt.Errorf("copy %q: %q: RANGE end %q is not a number", actName, key, parts[1])
		}
		r.end, r.hasEnd = end, true
	}
	return r, nil
}

// apply substitutes the current range values into a URL and header set.
func (p *restPagination) apply(rawURL string, hdr http.Header) string {
	for _, r := range p.ranges {
		tok := "{" + r.placeholder + "}"
		v := formatNumber(r.cur)
		switch r.where {
		case "AbsoluteUrl", "QueryParameters":
			rawURL = strings.ReplaceAll(rawURL, tok, v)
		case "Headers":
			for name, vals := range hdr {
				for i := range vals {
					if strings.Contains(vals[i], tok) {
						hdr.Set(name, strings.ReplaceAll(vals[i], tok, v))
					}
				}
			}
		}
	}
	return rawURL
}

// advance steps every range. It reports false when a bounded range is exhausted,
// which ends the loop.
func (p *restPagination) advance() bool {
	if len(p.ranges) == 0 {
		return true
	}
	for i := range p.ranges {
		r := &p.ranges[i]
		r.cur += r.step
		if r.hasEnd {
			if (r.step > 0 && r.cur > r.end) || (r.step < 0 && r.cur < r.end) {
				return false
			}
		}
	}
	return true
}

// formatNumber renders a range value the way a URL wants it: 1000, not 1000.0.
func formatNumber(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// resolveValue evaluates a pagination VALUE against the current response: a
// JSONPath into the body, or `Headers.x` / `headers.x` naming a response header.
// A missing or null result reports false, which is one of Fabric's documented
// stop conditions.
func resolveValue(expr string, doc any, hdr http.Header) (string, bool) {
	e := strings.TrimSpace(expr)
	switch {
	case strings.HasPrefix(e, "$"):
		v, ok := jsonPathLookup(doc, e)
		if !ok || v == nil {
			return "", false
		}
		return scalarString(v)
	case strings.HasPrefix(strings.ToLower(e), "headers."),
		strings.HasPrefix(strings.ToLower(e), "headers["):
		name, ok := headerSelector(e)
		if !ok {
			return "", false
		}
		v := hdr.Get(name)
		return v, v != ""
	}
	// A literal: Fabric's values are expressions, but a constant is harmless and
	// unambiguous (it cannot be mistaken for a path or a header).
	return e, e != ""
}

// headerSelector pulls the header name out of `Headers.x` or `Headers['x']`.
func headerSelector(e string) (string, bool) {
	rest := e[len("headers"):]
	switch {
	case strings.HasPrefix(rest, "."):
		return rest[1:], len(rest) > 1
	case strings.HasPrefix(rest, "['") && strings.HasSuffix(rest, "']"),
		strings.HasPrefix(rest, `["`) && strings.HasSuffix(rest, `"]`):
		return rest[2 : len(rest)-2], len(rest) > 4
	}
	return "", false
}

// scalarString renders a JSON scalar for use in a URL or header.
func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, t != ""
	case float64:
		return formatNumber(t), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

// done evaluates the EndCondition rules against the current response.
func (p *restPagination) done(doc any, hdr http.Header) bool {
	for _, ec := range p.endConds {
		var node any
		var present bool
		if strings.HasPrefix(strings.TrimSpace(ec.expr), "$") {
			node, present = jsonPathLookup(doc, ec.expr)
		} else if name, ok := headerSelector(strings.ToLower(ec.expr)); ok {
			v := hdr.Get(name)
			node, present = v, v != ""
		}
		cond := ec.cond
		switch {
		case strings.EqualFold(cond, "Exist"):
			if present {
				return true
			}
		case strings.EqualFold(cond, "NonExist"):
			if !present {
				return true
			}
		case strings.EqualFold(cond, "Empty"):
			if !present || isEmptyNode(node) {
				return true
			}
		case strings.HasPrefix(cond, "Const:"):
			want := strings.TrimPrefix(cond, "Const:")
			if present {
				if got, ok := scalarString(node); ok && got == want {
					return true
				}
			}
		}
	}
	return false
}

func isEmptyNode(node any) bool {
	switch t := node.(type) {
	case nil:
		return true
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	case string:
		return t == ""
	}
	return false
}

// nextRequest composes the following page's URL and headers, or reports false
// when nothing says there is one.
//
// Precedence follows Fabric's own: an explicit rule beats the RFC 5988 default,
// and the default only applies when the author declared no rules at all.
func (p *restPagination) nextRequest(curURL string, hdr http.Header, doc any, resp *http.Response) (string, bool) {
	if p.absoluteURL != "" {
		v, ok := resolveValue(p.absoluteURL, doc, resp.Header)
		if !ok {
			return "", false
		}
		return resolveRelative(curURL, v), true
	}

	changed := false
	next := curURL
	if len(p.queryParams) > 0 {
		u, err := url.Parse(curURL)
		if err != nil {
			return "", false
		}
		q := u.Query()
		for name, expr := range p.queryParams {
			v, ok := resolveValue(expr, doc, resp.Header)
			if !ok {
				return "", false
			}
			q.Set(name, v)
			changed = true
		}
		u.RawQuery = q.Encode()
		next = u.String()
	}
	for name, expr := range p.headers {
		v, ok := resolveValue(expr, doc, resp.Header)
		if !ok {
			return "", false
		}
		hdr.Set(name, v)
		changed = true
	}
	if len(p.ranges) > 0 {
		// The ranges themselves decide; advance() already reported exhaustion.
		return next, true
	}
	if changed {
		return next, true
	}

	// RFC 5988 is Fabric's DEFAULT only when no rule was declared: following Link
	// headers under someone's explicit rule set would page somewhere they never
	// asked to go. Writing `SupportRFC5988: true` alongside other rules is asking
	// for it, though, and that must still work.
	if p.rfc5988 && (!p.declared || p.rfc5988Explicit) {
		if link, ok := rfc5988Next(resp.Header.Values("Link")); ok {
			return resolveRelative(curURL, link), true
		}
	}
	return "", false
}

// rfc5988Next finds `<url>; rel="next"` in Link headers.
func rfc5988Next(links []string) (string, bool) {
	for _, header := range links {
		for _, part := range strings.Split(header, ",") {
			segs := strings.Split(part, ";")
			if len(segs) < 2 {
				continue
			}
			raw := strings.TrimSpace(segs[0])
			if !strings.HasPrefix(raw, "<") || !strings.HasSuffix(raw, ">") {
				continue
			}
			for _, s := range segs[1:] {
				kv := strings.SplitN(strings.TrimSpace(s), "=", 2)
				if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), "rel") &&
					strings.Trim(strings.TrimSpace(kv[1]), `"'`) == "next" {
					return raw[1 : len(raw)-1], true
				}
			}
		}
	}
	return "", false
}

// resolveRelative lets a next-link be relative, which Fabric documents
// ("either absolute URL or relative URL").
func resolveRelative(base, next string) string {
	if strings.HasPrefix(next, "http://") || strings.HasPrefix(next, "https://") {
		return next
	}
	b, err := url.Parse(base)
	if err != nil {
		return next
	}
	r, err := url.Parse(next)
	if err != nil {
		return next
	}
	return b.ResolveReference(r).String()
}
