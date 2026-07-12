package notebook

import "testing"

func TestParsePlainSingleCell(t *testing.T) {
	src := "# Fabric notebook source\nprint(\"hello from the emulator\")\n"
	cells := Parse([]byte(src))
	if len(cells) != 1 {
		t.Fatalf("cells = %d, want 1", len(cells))
	}
	c := cells[0]
	if c.Kind != Code || c.Language != "python" || c.Source != `print("hello from the emulator")` {
		t.Fatalf("bad cell: %+v", c)
	}
}

func TestParseMultiCellWithMagicsAndMarkdown(t *testing.T) {
	src := `# Fabric notebook source

# METADATA ********************

# META {
# META   "kernel_info": { "name": "synapse_pyspark" }
# META }

# CELL ********************

df = spark.range(3)
df.write.format("delta").save(path)

# MARKDOWN ********************

# MAGIC %md
# MAGIC ## Results

# CELL ********************

# MAGIC %%sql
# MAGIC SELECT count(*) FROM t

# CELL ********************

# MAGIC %%pyspark
# MAGIC print("done")

# METADATA ********************

# META {
# META   "language_group": "synapse_pyspark"
# META }
`
	cells := Parse([]byte(src))
	// Expect: code(python), markdown, code(sql), code(pyspark). Trailing META dropped.
	if len(cells) != 4 {
		t.Fatalf("cells = %d (%+v), want 4", len(cells), cells)
	}
	if cells[0].Kind != Code || cells[0].Language != "python" ||
		cells[0].Source != "df = spark.range(3)\ndf.write.format(\"delta\").save(path)" {
		t.Fatalf("cell0: %+v", cells[0])
	}
	if cells[1].Kind != Markdown || cells[1].Source != "%md\n## Results" {
		t.Fatalf("cell1 (markdown): %+v", cells[1])
	}
	if cells[2].Kind != Code || cells[2].Language != "sql" || cells[2].Source != "SELECT count(*) FROM t" {
		t.Fatalf("cell2 (sql): %+v", cells[2])
	}
	if cells[3].Kind != Code || cells[3].Language != "pyspark" || cells[3].Source != `print("done")` {
		t.Fatalf("cell3 (pyspark): %+v", cells[3])
	}
	// Indices are sequential.
	for i, c := range cells {
		if c.Index != i {
			t.Errorf("cell %d has index %d", i, c.Index)
		}
	}
	// CodeCells drops the markdown.
	if code := CodeCells(cells); len(code) != 3 {
		t.Fatalf("code cells = %d, want 3", len(code))
	}
}

func TestParseSkipsEmptyAndMetadataOnly(t *testing.T) {
	src := `# Fabric notebook source
# CELL ********************

# CELL ********************
x = 1
# METADATA ********************
# META { "a": 1 }
`
	cells := Parse([]byte(src))
	if len(cells) != 1 || cells[0].Source != "x = 1" {
		t.Fatalf("expected one non-empty code cell, got %+v", cells)
	}
}

func TestParseNoHeader(t *testing.T) {
	// A raw code cell with no Fabric header still parses.
	cells := Parse([]byte("spark.sql('SELECT 1')"))
	if len(cells) != 1 || cells[0].Source != "spark.sql('SELECT 1')" {
		t.Fatalf("got %+v", cells)
	}
}

func TestParseEmpty(t *testing.T) {
	if cells := Parse([]byte("# Fabric notebook source\n\n")); len(cells) != 0 {
		t.Fatalf("empty notebook should yield no cells, got %+v", cells)
	}
}
