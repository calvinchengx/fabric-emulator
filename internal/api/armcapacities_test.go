package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func TestARMCapacitiesRefresh(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	const armID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	feed := map[string]any{
		"generated": 1,
		"capacities": []map[string]any{
			{"id": ""}, // skipped
			{
				"id": armID, "armId": "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Fabric/capacities/azsdktest",
				"name": "azsdktest", "sku": "F8", "region": "westeurope", "state": "Active",
			},
		},
	}
	raw, _ := json.Marshal(feed)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_family/capacities" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)

	src := NewARMCapacities(st, srv.URL, false, srv.Client(), time.Second)
	if err := src.Refresh(); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetCapacity(armID)
	if err != nil || got.SKU != "F8" || got.Source != store.CapacitySourceARM || got.DisplayName != "azsdktest" {
		t.Fatalf("upserted = %+v %v", got, err)
	}
	seed, err := st.GetCapacity(store.DefaultCapacityID)
	if err != nil || seed.Source != store.CapacitySourceSeed {
		t.Fatalf("seeded default lost: %+v %v", seed, err)
	}

	// An empty feed drops ARM rows and keeps the seed.
	raw, _ = json.Marshal(map[string]any{"generated": 2, "capacities": []any{}})
	if err := src.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetCapacity(armID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ARM capacity survived an empty feed: %v", err)
	}
	if _, err := st.GetCapacity(store.DefaultCapacityID); err != nil {
		t.Fatalf("seed deleted: %v", err)
	}
}

func TestARMCapacitiesRefreshErrors(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	src := NewARMCapacities(st, "http://127.0.0.1:1", false, nil, 0)
	if src.TTL != 5*time.Second {
		t.Fatalf("ttl <= 0 should default to 5s, got %s", src.TTL)
	}
	if err := src.Refresh(); err == nil {
		t.Fatal("refresh against a closed port succeeded")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)
	src = NewARMCapacities(st, bad.URL, false, bad.Client(), time.Millisecond)
	if err := src.Refresh(); err == nil {
		t.Fatal("refresh against 500 succeeded")
	}

	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{nope"))
	}))
	t.Cleanup(malformed.Close)
	src = NewARMCapacities(st, malformed.URL, false, malformed.Client(), time.Millisecond)
	if err := src.Refresh(); err == nil {
		t.Fatal("refresh against malformed JSON succeeded")
	}

	// A closed store fails the upsert rather than leaving a half-applied feed.
	closed, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"generated": 1,
			"capacities": []map[string]any{{
				"id": "cccccccc-cccc-4ccc-8ccc-cccccccccccc", "name": "x", "sku": "F2",
			}},
		})
	}))
	t.Cleanup(ok.Close)
	src = NewARMCapacities(closed, ok.URL, false, ok.Client(), time.Millisecond)
	if err := src.Refresh(); err == nil {
		t.Fatal("refresh against a closed store succeeded")
	}

	// insecure=true builds a client; Run returns when done closes.
	insecure := NewARMCapacities(st, srvURL(t), true, nil, time.Hour)
	if insecure.Client == nil {
		t.Fatal("insecure client was nil")
	}
	done := make(chan struct{})
	close(done)
	insecure.Run(done)
}

func srvURL(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func TestARMCapacitiesRunStops(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"generated": n.Load(), "capacities": []any{}})
	}))
	t.Cleanup(srv.Close)
	src := NewARMCapacities(st, srv.URL, false, srv.Client(), 20*time.Millisecond)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		src.Run(done)
		close(finished)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && n.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	close(done)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after stop")
	}
	if n.Load() == 0 {
		t.Fatal("Run never polled")
	}
}
