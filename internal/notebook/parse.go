// Package notebook parses the Fabric notebook source format
// (`notebook-content.py`) into ordered, executable cells.
//
// Fabric stores a notebook as a Python-ish source file: a `# Fabric notebook
// source` header, then sections delimited by `# CELL ****`, `# MARKDOWN ****`,
// and `# METADATA ****` marker lines. Code can be written plainly or, for
// non-Python cells and cross-language magics, `# MAGIC `-prefixed — a leading
// `%%sql` / `%%pyspark` / … magic selects the cell language.
//
// The parser is pure and hermetic (no execution); an engine (real Spark, in the
// e2e) runs the code cells. Getting the split + language detection right in Go
// is the emulator's real contribution here.
package notebook

import (
	"regexp"
	"strings"
)

// Kind is a cell's type.
type Kind string

const (
	Code     Kind = "code"
	Markdown Kind = "markdown"
)

// Cell is one parsed notebook cell.
type Cell struct {
	Index    int    `json:"index"`
	Kind     Kind   `json:"kind"`
	Language string `json:"language,omitempty"` // code cells: python | pyspark | sql | scala | …
	Source   string `json:"source"`
}

var (
	delim = regexp.MustCompile(`^#\s+(CELL|MARKDOWN|METADATA)\b.*$`)
	magic = regexp.MustCompile(`^%%(\w+)`)
)

// Parse turns notebook source into ordered cells. Markdown and metadata are
// preserved/ignored appropriately; only code cells carry a language. Empty
// cells are dropped.
func Parse(src []byte) []Cell {
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")

	// Skip the leading `# Fabric notebook source` header, if present.
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	if start < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[start]), "# Fabric notebook source") {
		start++
	}

	// A notebook with no explicit CELL markers is a single code cell (the
	// simplest fabric-cicd shape).
	hasMarkers := false
	for _, l := range lines[start:] {
		if delim.MatchString(l) {
			hasMarkers = true
			break
		}
	}
	if !hasMarkers {
		return finish(nil, sectionKind("CELL"), lines[start:])
	}

	var cells []Cell
	var curKind string
	var buf []string
	for _, l := range lines[start:] {
		if m := delim.FindStringSubmatch(l); m != nil {
			cells = finish(cells, sectionKind(curKind), buf)
			curKind, buf = m[1], nil
			continue
		}
		if curKind == "" { // stray lines before the first marker → a code cell
			curKind = "CELL"
		}
		buf = append(buf, l)
	}
	return finish(cells, sectionKind(curKind), buf)
}

func sectionKind(marker string) Kind {
	switch marker {
	case "MARKDOWN":
		return Markdown
	default:
		return Code // CELL, or a leading unmarked block
	}
}

// finish materialises a section's buffered lines into a cell (dropping METADATA
// and empties), stamping the running index.
func finish(cells []Cell, kind Kind, buf []string) []Cell {
	if buf == nil {
		return cells
	}
	body, isMeta := stripMagics(buf)
	if isMeta {
		return cells
	}
	lang, src := "", strings.Trim(body, "\n")
	if kind == Code {
		lang, src = detectLanguage(src)
	}
	if strings.TrimSpace(src) == "" {
		return cells
	}
	return append(cells, Cell{Index: len(cells), Kind: kind, Language: lang, Source: src})
}

// stripMagics removes `# MAGIC ` / `# META ` decorations. `# META` lines are
// notebook metadata (skipped); a section that is *only* META lines is dropped.
func stripMagics(lines []string) (body string, metadataOnly bool) {
	var out []string
	sawContent, sawMeta := false, false
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "# META "):
			sawMeta = true
			continue
		case l == "# META" || l == "# METADATA":
			sawMeta = true
			continue
		case strings.HasPrefix(l, "# MAGIC "):
			out = append(out, l[len("# MAGIC "):])
			sawContent = true
		case l == "# MAGIC":
			out = append(out, "")
		default:
			out = append(out, l)
			if strings.TrimSpace(l) != "" {
				sawContent = true
			}
		}
	}
	return strings.Join(out, "\n"), sawMeta && !sawContent
}

// detectLanguage reads a leading `%%lang` / `%md` magic, returning the language
// and the source with that magic line removed. Defaults to python.
func detectLanguage(src string) (lang, rest string) {
	lines := strings.Split(src, "\n")
	// Find the first non-blank line.
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i < len(lines) {
		if m := magic.FindStringSubmatch(strings.TrimSpace(lines[i])); m != nil {
			lang := strings.ToLower(m[1])
			rest := strings.Join(append(append([]string{}, lines[:i]...), lines[i+1:]...), "\n")
			return normalizeLang(lang), strings.TrimSpace(rest)
		}
	}
	return "python", src
}

func normalizeLang(l string) string {
	switch l {
	case "spark":
		return "scala"
	case "python":
		return "python"
	default:
		return l // pyspark, sql, scala, csharp, configure, …
	}
}

// CodeCells returns only the executable code cells in order.
func CodeCells(cells []Cell) []Cell {
	var out []Cell
	for _, c := range cells {
		if c.Kind == Code {
			out = append(out, c)
		}
	}
	return out
}
