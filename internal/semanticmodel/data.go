package semanticmodel

import "encoding/json"

// Row is one table row: column name → value (numbers decode as float64, the
// evaluator's numeric type; text as string).
type Row map[string]any

// Data is the model's table rows, keyed by table name — import data the engine
// evaluates over. In real Fabric these come from Direct Lake (OneLake Delta) or
// import partitions; the emulator seeds them as a `data.json` definition part.
type Data map[string][]Row

// ParseData reads a `data.json` payload (`{"Store":[{…}], …}`) into Data. Any
// non-array value (e.g. a `_comment` key) is skipped, so the fixture's inline
// comments don't need stripping.
func ParseData(b []byte) (Data, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	d := Data{}
	for name, v := range raw {
		var rows []Row
		if json.Unmarshal(v, &rows) == nil {
			d[name] = rows
		}
	}
	return d, nil
}

// Rows returns the rows for a table (nil if none loaded).
func (d Data) Rows(table string) []Row { return d[table] }
