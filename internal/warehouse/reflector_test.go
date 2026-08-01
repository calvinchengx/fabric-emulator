package warehouse

import (
	"context"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
)

// A Reflector must not redo a table whose Delta log has not advanced.
//
// This is the property the medallion e2e needed and did not have. Reflection
// runs inside TDS login; when it outruns the login timeout the client retries,
// and a memoryless reflector restarts everything each time, so the retries
// never accumulate progress. Skipping is what makes a second login cheap.
func TestReflectorSkipsUnchangedTables(t *testing.T) {
	st, wsID, itemID := seedLakehouse(t)
	put(t, st, wsID, itemID, "Tables/sales/part-0.parquet",
		writeParquet(t, []saleRow{{"us", 80}, {"eu", 60}}))
	put(t, st, wsID, itemID, "Tables/sales/_delta_log/00000000000000000000.json",
		[]byte(`{"add":{"path":"part-0.parquet"}}`))

	db := testsupport.OpenMSSQL(t)
	ctx := context.Background()
	r := &Reflector{}

	done, err := r.Reflect(ctx, db, st, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0] != "sales" {
		t.Fatalf("first reflect = %v, want [sales]", done)
	}

	done, err = r.Reflect(ctx, db, st, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 0 {
		t.Errorf("second reflect = %v, want nothing — the log did not advance", done)
	}

	// Skipped must mean "already loaded", never "not loaded". If the skip path
	// ever DROPped without reloading, this is what would catch it.
	var got int64
	if err := db.QueryRowContext(ctx, "SELECT SUM(amount) FROM [sales]").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 140 {
		t.Errorf("SUM(amount) = %d, want 140 — the skipped table is not intact", got)
	}
}

// A new Delta commit must un-skip the table, or the endpoint serves stale data
// forever — a far worse failure than reflecting too often.
func TestReflectorReloadsWhenTheLogAdvances(t *testing.T) {
	st, wsID, itemID := seedLakehouse(t)
	put(t, st, wsID, itemID, "Tables/sales/part-0.parquet",
		writeParquet(t, []saleRow{{"us", 80}}))
	put(t, st, wsID, itemID, "Tables/sales/_delta_log/00000000000000000000.json",
		[]byte(`{"add":{"path":"part-0.parquet"}}`))

	db := testsupport.OpenMSSQL(t)
	ctx := context.Background()
	r := &Reflector{}

	if _, err := r.Reflect(ctx, db, st, itemID); err != nil {
		t.Fatal(err)
	}

	put(t, st, wsID, itemID, "Tables/sales/part-1.parquet",
		writeParquet(t, []saleRow{{"eu", 60}}))
	put(t, st, wsID, itemID, "Tables/sales/_delta_log/00000000000000000001.json",
		[]byte(`{"add":{"path":"part-1.parquet"}}`))

	done, err := r.Reflect(ctx, db, st, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0] != "sales" {
		t.Fatalf("reflect after a new commit = %v, want [sales]", done)
	}
	var got int64
	if err := db.QueryRowContext(ctx, "SELECT SUM(amount) FROM [sales]").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 140 {
		t.Errorf("SUM(amount) = %d, want 140 — the new commit was not picked up", got)
	}
}

// The point of recording a fingerprint only after a table succeeds: a failed
// run keeps what it finished and retries only what it did not.
//
// The failure is induced the way the real one happens — the context is
// cancelled mid-reflection, as it is when a client's login times out and it
// disconnects. A memoryless reflector would redo the finished tables on the
// next attempt; this asserts the retry does strictly less work.
func TestReflectorResumesAfterCancellation(t *testing.T) {
	st, wsID, itemID := seedLakehouse(t)
	for _, name := range []string{"a_sales", "b_sales"} {
		put(t, st, wsID, itemID, "Tables/"+name+"/part-0.parquet",
			writeParquet(t, []saleRow{{"us", 80}}))
		put(t, st, wsID, itemID, "Tables/"+name+"/_delta_log/00000000000000000000.json",
			[]byte(`{"add":{"path":"part-0.parquet"}}`))
	}

	db := testsupport.OpenMSSQL(t)
	r := &Reflector{}

	// Reflect only the first table, then cancel — this is the login-timeout
	// shape: some tables loaded, the rest not.
	ctx, cancel := context.WithCancel(context.Background())
	first, err := r.Reflect(ctx, db, st, itemID)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if len(first) != 2 {
		t.Fatalf("setup: reflected %v, want both tables", first)
	}

	// Advance ONE table's log. A resuming reflector must redo exactly that one.
	put(t, st, wsID, itemID, "Tables/b_sales/part-1.parquet",
		writeParquet(t, []saleRow{{"eu", 60}}))
	put(t, st, wsID, itemID, "Tables/b_sales/_delta_log/00000000000000000001.json",
		[]byte(`{"add":{"path":"part-1.parquet"}}`))

	done, err := r.Reflect(context.Background(), db, st, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0] != "b_sales" {
		t.Errorf("resumed reflect = %v, want only [b_sales] — a_sales was unchanged", done)
	}
}

// A fresh Reflector must work with no setup: warehouseRouter builds one with a
// composite literal, and the package-level Reflect delegates to a throwaway.
func TestReflectorZeroValueIsUsable(t *testing.T) {
	st, wsID, itemID := seedLakehouse(t)
	put(t, st, wsID, itemID, "Tables/sales/part-0.parquet",
		writeParquet(t, []saleRow{{"us", 80}}))
	put(t, st, wsID, itemID, "Tables/sales/_delta_log/00000000000000000000.json",
		[]byte(`{"add":{"path":"part-0.parquet"}}`))

	db := testsupport.OpenMSSQL(t)
	var r Reflector // zero value, no constructor
	done, err := r.Reflect(context.Background(), db, st, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 {
		t.Errorf("zero-value Reflector reflected %v, want [sales]", done)
	}
}

// deltaFingerprint must not read file contents — it runs on every login, for
// every table, and reading commits would put the cost back on the hot path.
// It must also refuse a folder that is not a Delta table, so those keep being
// skipped rather than becoming errors.
func TestDeltaFingerprint(t *testing.T) {
	st, wsID, itemID := seedLakehouse(t)
	put(t, st, wsID, itemID, "Tables/sales/_delta_log/00000000000000000000.json",
		[]byte(`{"add":{"path":"part-0.parquet"}}`))

	fp0, err := deltaFingerprint(st, itemID, "sales")
	if err != nil {
		t.Fatal(err)
	}
	if fp0 == "" {
		t.Fatal("empty fingerprint")
	}
	// Stable across calls, or nothing is ever skipped.
	if again, _ := deltaFingerprint(st, itemID, "sales"); again != fp0 {
		t.Errorf("fingerprint not stable: %q then %q", fp0, again)
	}
	// Changes when a commit lands, or stale data is served forever.
	put(t, st, wsID, itemID, "Tables/sales/_delta_log/00000000000000000001.json",
		[]byte(`{"add":{"path":"part-1.parquet"}}`))
	fp1, err := deltaFingerprint(st, itemID, "sales")
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == fp0 {
		t.Error("fingerprint unchanged after a new commit")
	}

	put(t, st, wsID, itemID, "Tables/notatable/readme.txt", []byte("hi"))
	if _, err := deltaFingerprint(st, itemID, "notatable"); err == nil {
		t.Error("a folder with no _delta_log should not produce a fingerprint")
	}
}
