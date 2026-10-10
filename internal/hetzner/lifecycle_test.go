package hetzner_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/hetzner"
)

const (
	liveLocation = "fsn1"
	liveImage    = "debian-13"
	// liveCap is how long the server may exist; it is deleted by then
	// whatever happened.
	liveCap = 10 * time.Minute
	// liveTestLabel marks the test's server, so that the final listing
	// finds it if it was left behind.
	liveTestLabel = "orchestrator-live-test"
)

// liveUserData is the smallest cloud-config: the API must accept it, and
// it changes nothing on the server but a marker file.
const liveUserData = "#cloud-config\nruncmd:\n  - [touch, /root/orchestrator-live-test]\n"

// hourlyGross returns the hourly price with VAT of serverType in
// location, or false when the price list has none.
func hourlyGross(pricing hetzner.Pricing, serverType, location string) (float64, bool) {
	for _, st := range pricing.ServerTypes {
		if st.Name != serverType {
			continue
		}
		for _, price := range st.Prices {
			if price.Location == location {
				gross, err := strconv.ParseFloat(price.PriceHourly.Gross, 64)
				return gross, err == nil
			}
		}
	}
	return 0, false
}

// serverLifecycle creates one server of the cheaper of cx23 and cax11 in
// fsn1 through client, waits for it to run, deletes it, waits for it to
// be gone, and checks that no server labelled as the test's is left. It
// polls every poll, and the server never outlives liveCap and a minute:
// it is deleted when the test ends however it ends.
func serverLifecycle(t *testing.T, client *hetzner.Client, poll time.Duration) {
	ctx, cancel := context.WithTimeout(t.Context(), liveCap)
	defer cancel()

	pricing, err := client.Pricing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type candidate struct {
		serverType string
		hourly     float64
	}
	var candidates []candidate
	for _, serverType := range []string{"cx23", "cax11"} {
		if hourly, ok := hourlyGross(pricing, serverType, liveLocation); ok {
			candidates = append(candidates, candidate{serverType, hourly})
		}
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int {
		switch {
		case a.hourly < b.hourly:
			return -1
		case a.hourly > b.hourly:
			return 1
		}
		return 0
	})
	if len(candidates) == 0 {
		t.Fatalf("the price list has neither cx23 nor cax11 in %s", liveLocation)
	}
	t.Logf("hourly prices in %s, %s with VAT: %+v", liveLocation, pricing.Currency, candidates)

	random := make([]byte, 4)
	rand.Read(random)
	run := hex.EncodeToString(random)
	labels := map[string]string{liveTestLabel: run}

	var server hetzner.Server
	deleted := false
	var deletedAt time.Time
	deleteServer := func() {
		if deleted || server.ID == 0 {
			return
		}
		deleteCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := client.DeleteServer(deleteCtx, server.ID); err != nil && !errors.Is(err, hetzner.ErrNotFound) {
			t.Errorf("delete server %d: %v; DELETE IT BY HAND", server.ID, err)
			return
		}
		deleted, deletedAt = true, time.Now()
		t.Logf("deleted server %d at %s", server.ID, deletedAt.Format(time.RFC3339))
	}
	// Registered before the first create, so that a server a failed
	// create made anyway is found by its label and deleted too.
	defer func() {
		deleteServer()
		listCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		strays, err := client.Servers(listCtx, liveTestLabel+"="+run)
		if err != nil {
			t.Errorf("list servers labelled %s=%s: %v", liveTestLabel, run, err)
		}
		for _, stray := range strays {
			if err := client.DeleteServer(listCtx, stray.ID); err != nil && !errors.Is(err, hetzner.ErrNotFound) {
				t.Errorf("delete stray server %d: %v; DELETE IT BY HAND", stray.ID, err)
			}
		}
		left, err := client.Servers(listCtx, liveTestLabel)
		if err != nil {
			t.Errorf("list servers labelled %s: %v", liveTestLabel, err)
			return
		}
		t.Logf("servers labelled %s after the test: %d", liveTestLabel, len(left))
		for _, s := range left {
			t.Errorf("server %d (%s, %s) is left; delete it by hand", s.ID, s.Name, s.Status)
		}
	}()

	// A create the API refuses makes no server, so the next candidate
	// still makes at most one.
	var chosen candidate
	for _, c := range candidates {
		server, err = client.CreateServer(ctx, hetzner.CreateServer{
			Name: "orch-live-" + run, ServerType: c.serverType, Image: liveImage, Location: liveLocation,
			Labels: labels, UserData: liveUserData,
		})
		if err == nil {
			chosen = c
			break
		}
		t.Logf("create %s: %v", c.serverType, err)
	}
	if err != nil {
		t.Fatal("no server created")
	}
	created := time.Now()
	t.Logf("created server %d (%s, %s, %s) at %s", server.ID, server.Name, chosen.serverType, liveLocation, created.Format(time.RFC3339))

	for {
		got, err := client.Server(ctx, server.ID)
		if err != nil {
			t.Fatalf("get server %d: %v", server.ID, err)
		}
		if got.Status == hetzner.StatusRunning {
			t.Logf("server %d running after %s: type %s, location %s, labels %v",
				got.ID, time.Since(created).Round(time.Second), got.ServerType.Name, got.Location.Name, got.Labels)
			if got.Labels[liveTestLabel] != run {
				t.Errorf("labels %v, want %s=%s", got.Labels, liveTestLabel, run)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("server %d not running within %s: %s", server.ID, liveCap, got.Status)
		case <-time.After(poll):
		}
	}

	deleteServer()
	for {
		_, err := client.Server(ctx, server.ID)
		if errors.Is(err, hetzner.ErrNotFound) {
			break
		}
		if err != nil {
			t.Fatalf("get deleted server %d: %v", server.ID, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("server %d still exists %s after its deletion", server.ID, time.Since(deletedAt))
		case <-time.After(poll):
		}
	}
	lifetime := deletedAt.Sub(created)
	t.Logf("server %d existed %s; at %.4f %s an hour that is %.4f %s by the minute, %.4f %s if billed per started hour",
		server.ID, lifetime.Round(time.Second), chosen.hourly, pricing.Currency,
		lifetime.Minutes()/60*chosen.hourly, pricing.Currency, chosen.hourly*float64(int(lifetime.Hours())+1), pricing.Currency)
}

// fakeCloud is a stateful stand-in for the API's servers and price
// list: a server runs from its second read, and is gone once deleted.
type fakeCloud struct {
	mu      sync.Mutex
	next    int64
	servers map[int64]*fakeCloudServer
	// refused are server types whose creation is refused as unavailable.
	refused map[string]bool
	created []string
}

type fakeCloudServer struct {
	req   hetzner.CreateServer
	reads int
}

func (f *fakeCloud) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	refuse := func(status int, code string) {
		reply(status, map[string]any{"error": map[string]any{"code": code, "message": code, "details": nil}})
	}
	view := func(id int64, s *fakeCloudServer) map[string]any {
		status := "initializing"
		if s.reads >= 2 {
			status = "running"
		}
		return map[string]any{"id": id, "name": s.req.Name, "status": status, "created": "2026-10-10T12:00:00Z", "labels": s.req.Labels,
			"server_type": map[string]any{"name": s.req.ServerType}, "location": map[string]any{"name": s.req.Location}}
	}
	id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/v1/servers/"), 10, 64)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/pricing":
		price := func(name, hourly string) map[string]any {
			return map[string]any{"name": name, "prices": []any{map[string]any{"location": "fsn1",
				"price_hourly": map[string]any{"net": hourly, "gross": hourly}, "price_monthly": map[string]any{"net": "1", "gross": "1"}}}}
		}
		reply(http.StatusOK, map[string]any{"pricing": map[string]any{"currency": "EUR", "vat_rate": "19.00",
			"server_types": []any{price("cx23", "0.0100"), price("cax11", "0.0090"), price("cpx22", "0.0010")}}})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/servers":
		var req hetzner.CreateServer
		json.NewDecoder(r.Body).Decode(&req)
		f.created = append(f.created, req.ServerType)
		if f.refused[req.ServerType] {
			refuse(http.StatusPreconditionFailed, "resource_unavailable")
			return
		}
		f.next++
		f.servers[f.next] = &fakeCloudServer{req: req}
		reply(http.StatusCreated, map[string]any{"server": view(f.next, f.servers[f.next])})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/servers":
		key, value, hasValue := strings.Cut(r.URL.Query().Get("label_selector"), "=")
		list := []any{}
		for id, s := range f.servers {
			got, ok := s.req.Labels[key]
			if ok && (!hasValue || got == value) {
				list = append(list, view(id, s))
			}
		}
		reply(http.StatusOK, map[string]any{"servers": list, "meta": map[string]any{"pagination": map[string]any{"page": 1, "next_page": nil}}})
	case r.Method == http.MethodGet && id != 0:
		s, ok := f.servers[id]
		if !ok {
			refuse(http.StatusNotFound, "not_found")
			return
		}
		s.reads++
		reply(http.StatusOK, map[string]any{"server": view(id, s)})
	case r.Method == http.MethodDelete && id != 0:
		if _, ok := f.servers[id]; !ok {
			refuse(http.StatusNotFound, "not_found")
			return
		}
		delete(f.servers, id)
		reply(http.StatusOK, map[string]any{"action": map[string]any{"id": 1, "command": "delete_server", "status": "running"}})
	default:
		refuse(http.StatusNotFound, "not_found")
	}
}

func TestGivenFakeCloudWhenTheLiveLifecycleRunsThenTheCheaperAvailableServerRunsAndIsDeleted(t *testing.T) {
	cloud := &fakeCloud{servers: make(map[int64]*fakeCloudServer), refused: map[string]bool{"cax11": true}}
	srv := httptest.NewServer(http.HandlerFunc(cloud.serve))
	defer srv.Close()

	serverLifecycle(t, hetzner.NewClient(srv.URL+"/v1", testToken, nil), time.Millisecond)

	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if !slices.Equal(cloud.created, []string{"cax11", "cx23"}) {
		t.Errorf("creates %v, want the cheaper cax11 refused, then cx23", cloud.created)
	}
	if len(cloud.servers) != 0 {
		t.Errorf("%d servers left", len(cloud.servers))
	}
}
