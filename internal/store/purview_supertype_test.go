package store

// ListEntitiesBySuperType is what makes derived lineage real rather than a
// query for type_name = 'Process'. A real model subclasses Process; a query
// that only matches the base type returns empty lineage that looks like
// "nothing yet" rather than like a bug.

import (
	"encoding/json"
	"testing"
)

func putType(t *testing.T, s *Store, name string, supers []string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"name": name, "superTypes": supers})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTypeDef(&AtlasTypeDef{Name: name, Category: "ENTITY", Body: body}); err != nil {
		t.Fatal(err)
	}
}

func putEntity(t *testing.T, s *Store, typeName, qname, status string) *AtlasEntityRow {
	t.Helper()
	row := &AtlasEntityRow{
		TypeName:      typeName,
		QualifiedName: qname,
		Status:        status,
		Body:          json.RawMessage(`{"typeName":"` + typeName + `"}`),
	}
	if _, err := s.PutEntity(row); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestListEntitiesBySuperTypeIncludesDescendantsAndSkipsDeleted(t *testing.T) {
	s := newTestStore(t)
	putType(t, s, "Process", nil)
	putType(t, s, "CopyJob", []string{"Process"})
	putType(t, s, "DataSet", nil)

	live := putEntity(t, s, "CopyJob", "job://live", "ACTIVE")
	putEntity(t, s, "CopyJob", "job://gone", "DELETED")
	base := putEntity(t, s, "Process", "job://base", "ACTIVE")
	putEntity(t, s, "DataSet", "lake://table", "ACTIVE")

	got, err := s.ListEntitiesBySuperType("Process")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, e := range got {
		seen[e.QualifiedName] = e.TypeName
	}
	if seen[live.QualifiedName] != "CopyJob" {
		t.Fatalf("CopyJob subclass missing: %v", seen)
	}
	if seen[base.QualifiedName] != "Process" {
		t.Fatalf("base Process missing: %v", seen)
	}
	if _, ok := seen["job://gone"]; ok {
		t.Fatalf("DELETED process was returned: %v", seen)
	}
	if _, ok := seen["lake://table"]; ok {
		t.Fatalf("DataSet is not a Process descendant: %v", seen)
	}
}

// A cyclic superTypes registration must not spin. The registration path
// should refuse one, but a store written by an older build cannot hang here.
func TestTypeAndDescendantsTerminatesOnACycle(t *testing.T) {
	s := newTestStore(t)
	putType(t, s, "A", []string{"B"})
	putType(t, s, "B", []string{"A"})
	names, err := s.typeAndDescendants("A")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("cycle produced no names — A should still match itself")
	}
}
