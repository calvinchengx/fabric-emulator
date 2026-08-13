package api

// ARM capacity feed consumer: when arm-emulator is wired in, capacities
// created over Microsoft.Fabric/capacities appear on GET /v1/capacities.
// Azure's ARM→Fabric sync is internal, so this poll of /_family/capacities
// is the honest localhost equivalent — the same exception the Key Vault
// sibling makes for /_family/authorization.

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

type armCapacityFeed struct {
	Generated  int64 `json:"generated"`
	Capacities []struct {
		ID          string `json:"id"`
		ARMID       string `json:"armId"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		SKU         string `json:"sku"`
		Region      string `json:"region"`
		State       string `json:"state"`
	} `json:"capacities"`
}

// ARMCapacities polls arm-emulator's capacities feed and upserts those
// rows into the local store. The seeded default stays; ARM rows come and
// go with the feed.
type ARMCapacities struct {
	BaseURL string
	Client  *http.Client
	TTL     time.Duration
	Store   *store.Store

	mu   sync.Mutex
	last int64
}

// NewARMCapacities wires a feed consumer. insecure skips TLS verification
// for arm-emulator's self-signed cert; client overrides when non-nil (tests).
func NewARMCapacities(st *store.Store, baseURL string, insecure bool, client *http.Client, ttl time.Duration) *ARMCapacities {
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if insecure {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		client = &http.Client{Transport: tr}
	}
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	return &ARMCapacities{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Client:  client,
		TTL:     ttl,
		Store:   st,
	}
}

// Refresh fetches the feed once and applies it.
func (a *ARMCapacities) Refresh() error {
	resp, err := a.Client.Get(a.BaseURL + "/_family/capacities")
	if err != nil {
		return fmt.Errorf("fetch ARM capacities feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch ARM capacities feed: status %d", resp.StatusCode)
	}
	var f armCapacityFeed
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		return fmt.Errorf("parse ARM capacities feed: %w", err)
	}
	seen := map[string]bool{}
	for _, c := range f.Capacities {
		if c.ID == "" {
			continue
		}
		seen[c.ID] = true
		name := c.DisplayName
		if name == "" {
			name = c.Name
		}
		state := c.State
		if state == "" {
			state = "Active"
		}
		if err := a.Store.PutCapacity(&store.Capacity{
			ID: c.ID, DisplayName: name, SKU: c.SKU, Region: c.Region,
			State: state, Source: store.CapacitySourceARM, ARMID: c.ARMID,
		}); err != nil {
			return err
		}
	}
	all, err := a.Store.ListCapacities()
	if err != nil {
		return err
	}
	for _, c := range all {
		if c.Source != store.CapacitySourceARM || seen[c.ID] {
			continue
		}
		if err := a.Store.DeleteCapacity(c.ID); err != nil {
			return err
		}
	}
	a.mu.Lock()
	a.last = f.Generated
	a.mu.Unlock()
	return nil
}

// Run refreshes until done closes. A transient ARM outage leaves the
// last-known capacities in place.
func (a *ARMCapacities) Run(done <-chan struct{}) {
	t := time.NewTicker(a.TTL)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			_ = a.Refresh()
		}
	}
}
