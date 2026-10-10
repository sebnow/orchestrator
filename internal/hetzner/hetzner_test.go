package hetzner_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sebnow/orchestrator/internal/hetzner"
)

const testToken = "test-token-not-a-secret"

// recorded is a request the fake API received.
type recorded struct {
	method, path, query, auth, contentType string
	body                                   []byte
}

// fakeAPI answers each "METHOD /path" in routes with the fixture's
// status and the file under testdata, and records every request.
type fakeAPI struct {
	url string
	mu  sync.Mutex
	got []recorded
}

type answer struct {
	status  int
	fixture string
}

func startFakeAPI(t *testing.T, routes map[string]answer) *fakeAPI {
	t.Helper()
	api := &fakeAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		api.mu.Lock()
		api.got = append(api.got, recorded{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"), body: body})
		api.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		if page := r.URL.Query().Get("page"); page != "" {
			key += "?page=" + page
		}
		route, ok := routes[key]
		if !ok {
			t.Errorf("unexpected request %s", key)
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(route.status)
		if route.fixture != "" {
			data, err := os.ReadFile(filepath.Join("testdata", route.fixture))
			if err != nil {
				t.Error(err)
			}
			w.Write(data)
		}
	}))
	t.Cleanup(srv.Close)
	api.url = srv.URL + "/v1"
	return api
}

func (a *fakeAPI) requests() []recorded {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]recorded(nil), a.got...)
}

func (a *fakeAPI) client() *hetzner.Client {
	return hetzner.NewClient(a.url, testToken, nil)
}

func TestGivenServerToCreateWhenCreatedThenTheRequestCarriesItAndTheServerIsReturned(t *testing.T) {
	api := startFakeAPI(t, map[string]answer{"POST /v1/servers": {http.StatusCreated, "create_server.json"}})

	server, err := api.client().CreateServer(t.Context(), hetzner.CreateServer{
		Name: "vps-ab12cd34", ServerType: "cx23", Image: "debian-13", Location: "fsn1",
		Labels:   map[string]string{"orchestrator": "1", "daemon": "vps-ab12cd34"},
		UserData: "#cloud-config\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if server.ID != 42 || server.Status != hetzner.StatusInitializing || server.ServerType.Name != "cpx22" || server.Location.Name != "fsn1" {
		t.Errorf("server = %+v", server)
	}
	if server.PublicNet.IPv4 == nil || server.PublicNet.IPv4.IP != "1.2.3.4" {
		t.Errorf("public IPv4 = %+v", server.PublicNet.IPv4)
	}
	req := api.requests()[0]
	if req.auth != "Bearer "+testToken || req.contentType != "application/json" {
		t.Errorf("Authorization %q, Content-Type %q", req.auth, req.contentType)
	}
	var sent map[string]any
	if err := json.Unmarshal(req.body, &sent); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"name": "vps-ab12cd34", "server_type": "cx23", "image": "debian-13", "location": "fsn1",
		"labels":    map[string]any{"orchestrator": "1", "daemon": "vps-ab12cd34"},
		"user_data": "#cloud-config\n",
	}
	if got, _ := json.Marshal(sent); string(got) != string(mustJSON(t, want)) {
		t.Errorf("body = %s, want %s", got, mustJSON(t, want))
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestGivenSSHKeysWhenCreatingThenTheyAreSent(t *testing.T) {
	api := startFakeAPI(t, map[string]answer{"POST /v1/servers": {http.StatusCreated, "create_server.json"}})

	_, err := api.client().CreateServer(t.Context(), hetzner.CreateServer{Name: "a", ServerType: "cx23", Image: "debian-13", SSHKeys: []string{"owner"}})
	if err != nil {
		t.Fatal(err)
	}
	if body := string(api.requests()[0].body); !strings.Contains(body, `"ssh_keys":["owner"]`) {
		t.Errorf("body = %s, want ssh_keys", body)
	}
}

func TestGivenServerWhenReadThenItsStatusAndLabelsAreReturned(t *testing.T) {
	api := startFakeAPI(t, map[string]answer{"GET /v1/servers/42": {http.StatusOK, "server.json"}})

	server, err := api.client().Server(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if server.Status != hetzner.StatusRunning || server.Labels["daemon"] != "vps-ab12cd34" || server.Created.IsZero() {
		t.Errorf("server = %+v", server)
	}
}

func TestGivenMissingServerWhenReadOrDeletedThenErrNotFound(t *testing.T) {
	api := startFakeAPI(t, map[string]answer{
		"GET /v1/servers/42":    {http.StatusNotFound, "error_not_found.json"},
		"DELETE /v1/servers/42": {http.StatusNotFound, "error_not_found.json"},
	})

	if _, err := api.client().Server(t.Context(), 42); !errors.Is(err, hetzner.ErrNotFound) {
		t.Errorf("get: err = %v, want ErrNotFound", err)
	}
	err := api.client().DeleteServer(t.Context(), 42)
	if !errors.Is(err, hetzner.ErrNotFound) {
		t.Errorf("delete: err = %v, want ErrNotFound", err)
	}
	if apiErr, ok := errors.AsType[*hetzner.APIError](err); !ok || apiErr.Code != "not_found" || apiErr.Message != "server with ID '42' not found" {
		t.Errorf("delete: API error = %+v", apiErr)
	}
}

func TestGivenServerWhenDeletedThenTheAPIIsAskedToDeleteIt(t *testing.T) {
	api := startFakeAPI(t, map[string]answer{"DELETE /v1/servers/42": {http.StatusOK, "delete_server.json"}})

	if err := api.client().DeleteServer(t.Context(), 42); err != nil {
		t.Fatal(err)
	}
	if got := api.requests(); len(got) != 1 || got[0].auth != "Bearer "+testToken {
		t.Errorf("requests = %+v", got)
	}
}

func TestGivenTwoPagesOfServersWhenListedByLabelThenBothPagesAreReturned(t *testing.T) {
	api := startFakeAPI(t, map[string]answer{
		"GET /v1/servers?page=1": {http.StatusOK, "servers_page1.json"},
		"GET /v1/servers?page=2": {http.StatusOK, "servers_page2.json"},
	})

	servers, err := api.client().Servers(t.Context(), "orchestrator=1")
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 || servers[0].ID != 42 || servers[1].ID != 43 {
		t.Errorf("servers = %+v", servers)
	}
	for _, req := range api.requests() {
		if !strings.Contains(req.query, "label_selector=orchestrator%3D1") {
			t.Errorf("query %q has no label selector", req.query)
		}
	}
}

func TestGivenPriceListWhenReadThenEachServerTypesHourlyPriceIsReturned(t *testing.T) {
	api := startFakeAPI(t, map[string]answer{"GET /v1/pricing": {http.StatusOK, "pricing.json"}})

	pricing, err := api.client().Pricing(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if pricing.Currency != "EUR" || len(pricing.ServerTypes) != 1 || pricing.ServerTypes[0].Name != "cpx22" {
		t.Fatalf("pricing = %+v", pricing)
	}
	price := pricing.ServerTypes[0].Prices[0]
	if price.Location != "fsn1" || price.PriceHourly != (hetzner.Price{Net: "1.0000", Gross: "1.1900"}) {
		t.Errorf("price = %+v", price)
	}
}

func TestGivenRefusalsWhenCallingThenTheErrorSaysWhetherToRetryAndNeverHoldsTheToken(t *testing.T) {
	api := startFakeAPI(t, map[string]answer{
		"GET /v1/servers/1": {http.StatusUnauthorized, "error_unauthorized.json"},
		"GET /v1/servers/2": {http.StatusLocked, "error_locked.json"},
		"GET /v1/servers/3": {http.StatusTooManyRequests, ""},
		"GET /v1/servers/4": {http.StatusBadGateway, ""},
	})
	for id, transient := range map[hetzner.ServerID]bool{1: false, 2: true, 3: true, 4: true} {
		_, err := api.client().Server(t.Context(), id)
		if err == nil {
			t.Fatalf("server %d: no error", id)
		}
		if errors.Is(err, hetzner.ErrTransient) != transient {
			t.Errorf("server %d: err = %v, transient %v", id, err, !transient)
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("server %d: error %q holds the token", id, err)
		}
	}
}

func TestGivenTokenFileWhenLoadedThenOnlyAnOwnerOnlyFileIsAccepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hetzner-token")
	if err := os.WriteFile(path, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err := hetzner.LoadToken(path); err != nil || token != testToken {
		t.Errorf("0600: token %q, %v", token, err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := hetzner.LoadToken(path); err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("0640: err = %v, want a refusal without the token", err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hetzner.LoadToken(empty); err == nil {
		t.Error("empty file: no error")
	}
}
