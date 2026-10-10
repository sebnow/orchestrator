package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sebnow/orchestrator/internal/hetzner"
	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// fakeHetzner is the parts of the Hetzner Cloud API that provisioning
// calls, answering in the API's shapes (https://docs.hetzner.cloud/reference/cloud).
type fakeHetzner struct {
	url string

	mu      sync.Mutex
	nextID  int64
	servers map[int64]hetzner.CreateServer
	created []hetzner.CreateServer
	deleted []int64
	// refuseCreate, when set, is the error code a create gets with 403.
	refuseCreate string
}

func startFakeHetzner(t *testing.T) *fakeHetzner {
	t.Helper()
	api := &fakeHetzner{nextID: 42, servers: make(map[int64]hetzner.CreateServer)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/servers", func(w http.ResponseWriter, r *http.Request) {
		var req hetzner.CreateServer
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode create: %v", err)
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		api.created = append(api.created, req)
		if api.refuseCreate != "" {
			writeHetznerError(w, http.StatusForbidden, api.refuseCreate)
			return
		}
		id := api.nextID
		api.nextID++
		api.servers[id] = req
		writeJSON(w, http.StatusCreated, map[string]any{
			"server":        hetznerServer(id, req, "initializing"),
			"action":        map[string]any{"id": 1, "command": "create_server", "status": "running", "progress": 0, "started": "2026-10-10T12:00:00Z", "finished": nil, "resources": []any{map[string]any{"id": id, "type": "server"}}, "error": nil},
			"next_actions":  []any{},
			"root_password": "not-used",
		})
	})
	mux.HandleFunc("GET /v1/servers", func(w http.ResponseWriter, r *http.Request) {
		selector := r.URL.Query().Get("label_selector")
		api.mu.Lock()
		defer api.mu.Unlock()
		list := []any{}
		for id, server := range api.servers {
			if matchesSelector(server.Labels, selector) {
				list = append(list, hetznerServer(id, server, "running"))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"servers": list, "meta": map[string]any{"pagination": map[string]any{"page": 1, "per_page": 50, "previous_page": nil, "next_page": nil, "last_page": 1, "total_entries": len(list)}}})
	})
	mux.HandleFunc("DELETE /v1/servers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		api.mu.Lock()
		defer api.mu.Unlock()
		if _, ok := api.servers[id]; !ok {
			writeHetznerError(w, http.StatusNotFound, "not_found")
			return
		}
		delete(api.servers, id)
		api.deleted = append(api.deleted, id)
		writeJSON(w, http.StatusOK, map[string]any{"action": map[string]any{"id": 2, "command": "delete_server", "status": "running", "progress": 0, "started": "2026-10-10T12:00:00Z", "finished": nil, "resources": []any{map[string]any{"id": id, "type": "server"}}, "error": nil}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	api.url = srv.URL + "/v1"
	return api
}

func hetznerServer(id int64, req hetzner.CreateServer, status string) map[string]any {
	return map[string]any{
		"id": id, "name": req.Name, "status": status, "created": "2026-10-10T12:00:00Z", "labels": req.Labels,
		"server_type": map[string]any{"name": req.ServerType, "architecture": "x86"},
		"location":    map[string]any{"name": req.Location},
		"public_net":  map[string]any{"ipv4": map[string]any{"ip": "192.0.2.10"}},
	}
}

func writeHetznerError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": code + " for the test", "details": nil}})
}

// matchesSelector reports whether labels match selector, a list of
// key=value pairs.
func matchesSelector(labels map[string]string, selector string) bool {
	for _, term := range strings.Split(selector, ",") {
		key, value, _ := strings.Cut(term, "=")
		if labels[key] != value {
			return false
		}
	}
	return true
}

func (f *fakeHetzner) state() (created []hetzner.CreateServer, live int, deleted []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hetzner.CreateServer(nil), f.created...), len(f.servers), append([]int64(nil), f.deleted...)
}

// startProvisioningServer is the insecure test server with provisioning
// through api.
func startProvisioningServer(t *testing.T, api *fakeHetzner) (testServer, *pki.CA) {
	t.Helper()
	ca, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	return startTestServerWith(t, Options{CA: ca, Provisioning: &Provisioning{
		Hetzner:    hetzner.NewClient(api.url, "test-token", nil),
		ServerType: "cx23", Location: "fsn1", Image: "debian-13",
		DaemonURL: "https://downloads.example/daemon-linux-amd64", PublicURL: "https://orchestrator.example:8443",
	}}), ca
}

func provisionVPS(t *testing.T, srv testServer) vpsView {
	t.Helper()
	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/provision", "")
	if status != http.StatusCreated {
		t.Fatalf("provision: status %d %s", status, body)
	}
	var view vpsView
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func listVPSes(t *testing.T, srv testServer) []vpsView {
	t.Helper()
	status, body := doRequest(t, http.MethodGet, srv.url+"/v1/vpses", "")
	if status != http.StatusOK {
		t.Fatalf("list: status %d %s", status, body)
	}
	var views []vpsView
	if err := json.Unmarshal([]byte(body), &views); err != nil {
		t.Fatal(err)
	}
	return views
}

var enrolTokenFlag = regexp.MustCompile(`-enrol-token (\S+)`)

// enrolWith enrols token's daemon over plain HTTP and returns the status.
func enrolWith(t *testing.T, srv testServer, token string) int {
	t.Helper()
	parsed, err := protocol.ParseEnrolmentToken(token)
	if err != nil {
		t.Fatal(err)
	}
	_, csrPEM, err := pki.NewDaemonRequest(parsed.Daemon)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(protocol.Enrolment{Token: token, CSR: string(csrPEM)})
	status, _ := doRequest(t, http.MethodPost, srv.url+protocol.EnrolPath, string(body))
	return status
}

func TestGivenProvisioningWhenTheOwnerProvisionsThenTheVPSIsCreatedAndItsDaemonEnrolsAndConnects(t *testing.T) {
	api := startFakeHetzner(t)
	srv, ca := startProvisioningServer(t, api)

	view := provisionVPS(t, srv)

	if !regexp.MustCompile(`^vps-[a-z2-7]{8}$`).MatchString(string(view.Daemon)) {
		t.Errorf("daemon id %q, want vps- and eight base32 characters", view.Daemon)
	}
	if view.ServerID != 42 || view.State != vpsCreating || view.ServerType != "cx23" || view.Location != "fsn1" {
		t.Errorf("view = %+v", view)
	}
	created, _, _ := api.state()
	if len(created) != 1 {
		t.Fatalf("%d servers created, want 1", len(created))
	}
	req := created[0]
	if req.Name != string(view.Daemon) || req.ServerType != "cx23" || req.Image != "debian-13" || req.Location != "fsn1" ||
		req.Labels["orchestrator"] != "1" || req.Labels["daemon"] != string(view.Daemon) || len(req.SSHKeys) != 0 {
		t.Errorf("create = %+v", req)
	}
	if caLine := strings.Split(string(ca.CertPEM()), "\n")[1]; strings.Contains(req.UserData, "PRIVATE KEY") || !strings.Contains(req.UserData, caLine) {
		t.Errorf("user data holds a private key or lacks the CA:\n%s", req.UserData)
	}
	match := enrolTokenFlag.FindStringSubmatch(req.UserData)
	if match == nil || !strings.HasPrefix(match[1], string(view.Daemon)+":") {
		t.Fatalf("user data has no enrolment token for %s:\n%s", view.Daemon, req.UserData)
	}

	if status := enrolWith(t, srv, match[1]); status != http.StatusOK {
		t.Fatalf("enrol with the user data's token: status %d", status)
	}
	if got := listVPSes(t, srv); len(got) != 1 || got[0].State != vpsEnrolled {
		t.Errorf("after enrolment: %+v, want enrolled", got)
	}
	openCommandStream(t, srv, view.Daemon, "")
	if got := listVPSes(t, srv); len(got) != 1 || got[0].State != vpsConnected {
		t.Errorf("after connecting: %+v, want connected", got)
	}
}

func TestGivenHetznerRefusesWhenTheOwnerProvisionsThenNothingIsRecorded(t *testing.T) {
	api := startFakeHetzner(t)
	api.refuseCreate = "resource_limit_exceeded"
	srv, _ := startProvisioningServer(t, api)

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/provision", "")

	if status != http.StatusBadGateway || !strings.Contains(body, "resource_limit_exceeded") || strings.Contains(body, "test-token") {
		t.Errorf("status %d %q, want 502 with Hetzner's code and no token", status, body)
	}
	if got := listVPSes(t, srv); len(got) != 0 {
		t.Errorf("VPSes = %+v, want none", got)
	}
	var tokens int
	if err := srv.store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM enrolment_tokens`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 {
		t.Errorf("%d enrolment tokens kept, want 0", tokens)
	}
}

func TestGivenVPSWhenTheOwnerDestroysItThenItsServerIsDeletedAndItsTokenSpent(t *testing.T) {
	api := startFakeHetzner(t)
	srv, _ := startProvisioningServer(t, api)
	view := provisionVPS(t, srv)
	created, _, _ := api.state()
	token := enrolTokenFlag.FindStringSubmatch(created[0].UserData)[1]

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/daemons/"+string(view.Daemon)+"/destroy", "")

	if status != http.StatusOK || !strings.Contains(body, `"state":"destroyed"`) {
		t.Fatalf("destroy: status %d %s", status, body)
	}
	if _, live, deleted := api.state(); live != 0 || len(deleted) != 1 || deleted[0] != 42 {
		t.Errorf("live %d, deleted %v; want server 42 deleted", live, deleted)
	}
	if status := enrolWith(t, srv, token); status != http.StatusUnauthorized {
		t.Errorf("enrol after destroy: status %d, want 401", status)
	}
	if status, _ := doRequest(t, http.MethodPost, srv.url+"/v1/daemons/"+string(view.Daemon)+"/destroy", ""); status != http.StatusConflict {
		t.Errorf("destroy again: status %d, want 409", status)
	}
	if status, _ := doRequest(t, http.MethodPost, srv.url+"/v1/daemons/laptop/destroy", ""); status != http.StatusNotFound {
		t.Errorf("destroy a daemon that is no VPS: status %d, want 404", status)
	}
}

func TestGivenServerAlreadyGoneAtHetznerWhenTheOwnerDestroysTheVPSThenItIsDestroyed(t *testing.T) {
	api := startFakeHetzner(t)
	srv, _ := startProvisioningServer(t, api)
	view := provisionVPS(t, srv)
	api.mu.Lock()
	clear(api.servers)
	api.mu.Unlock()

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/daemons/"+string(view.Daemon)+"/destroy", "")

	if status != http.StatusOK || !strings.Contains(body, `"state":"destroyed"`) {
		t.Errorf("destroy: status %d %s, want destroyed", status, body)
	}
}

func TestGivenNoHetznerTokenWhenTheOwnerProvisionsThenProvisioningIsOff(t *testing.T) {
	srv := startTestServer(t)

	if status, _ := doRequest(t, http.MethodPost, srv.url+"/v1/provision", ""); status != http.StatusNotFound {
		t.Errorf("provision: status %d, want 404", status)
	}
	if page := send(t, http.MethodGet, srv.url+"/", nil, false); !strings.Contains(page.body, "Provisioning is off") {
		t.Errorf("dashboard does not say provisioning is off")
	}
}

func TestGivenProvisioningWhenTheOwnerUsesTheDashboardThenVPSesAreProvisionedListedAndDestroyed(t *testing.T) {
	api := startFakeHetzner(t)
	srv, _ := startProvisioningServer(t, api)

	got := send(t, http.MethodPost, srv.url+"/vpses", url.Values{}, false)
	if got.status != http.StatusSeeOther || got.header.Get("Location") != "/" {
		t.Fatalf("provision form: status %d, Location %q", got.status, got.header.Get("Location"))
	}
	views := listVPSes(t, srv)
	if len(views) != 1 {
		t.Fatalf("VPSes = %+v", views)
	}
	daemon := string(views[0].Daemon)
	page := send(t, http.MethodGet, srv.url+"/", nil, false).body
	for _, want := range []string{"Provision a VPS", daemon, "creating", `action="/daemons/` + daemon + `/destroy"`} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}

	got = send(t, http.MethodPost, srv.url+"/daemons/"+daemon+"/destroy", url.Values{}, false)
	if got.status != http.StatusSeeOther {
		t.Fatalf("destroy form: status %d %s", got.status, got.body)
	}
	if page := send(t, http.MethodGet, srv.url+"/", nil, false).body; strings.Contains(page, daemon) {
		t.Errorf("dashboard still lists destroyed %s", daemon)
	}
}

func TestGivenHetznerRefusesWhenTheOwnerProvisionsFromTheDashboardThenTheFailureIsShown(t *testing.T) {
	api := startFakeHetzner(t)
	api.refuseCreate = "forbidden"
	srv, _ := startProvisioningServer(t, api)

	got := send(t, http.MethodPost, srv.url+"/vpses", url.Values{}, false)

	if got.status != http.StatusBadGateway || !strings.Contains(got.body, "Provisioning failed") || !strings.Contains(got.body, "forbidden") {
		t.Errorf("status %d, body %s", got.status, got.body)
	}
}

func TestGivenUserDataWhenRenderedThenItHoldsTheTokenTheCAAndTheHarnessUserSetUp(t *testing.T) {
	ca, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	token := protocol.EnrolmentToken{Daemon: "vps-abcdefgh", Secret: newSecret()}
	data, err := renderUserData(userDataInput{Token: token, CA: ca.CertPEM(), ServerURL: "https://orchestrator.example:8443", DaemonURL: "https://downloads.example/daemon?a=1&b=2"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#cloud-config\n",
		"-server https://orchestrator.example:8443 -ca /etc/orchestrator/ca.crt -enrol-token " + token.String() + " ",
		"-harness-user orch-agent -workspace-dir /srv/orchestrator/workspaces -claude /usr/local/bin/claude",
		"      " + strings.ReplaceAll(strings.TrimSpace(string(ca.CertPEM())), "\n", "\n      ") + "\n",
		"Defaults!/usr/local/bin/claude !secure_path\n",
		"orchestrator ALL = (orch-agent) CWD=* NOPASSWD: ORCH_HARNESS\n",
		"useradd --create-home --shell /bin/bash orch-agent\n",
		`curl -fsSL -o /usr/local/bin/orchestrator-daemon.download "https://downloads.example/daemon?a=1&b=2"`,
		"claude-code-releases/" + claudeVersion,
		"sha256sum -c -",
	} {
		if !strings.Contains(data, want) {
			t.Errorf("user data lacks %q:\n%s", want, data)
		}
	}
}

func TestGivenDaemonURLOnTheServerWhenRenderingUserDataThenTheDownloadIsVerifiedAgainstTheCA(t *testing.T) {
	token := protocol.EnrolmentToken{Daemon: "vps-abcdefgh", Secret: newSecret()}
	for url, want := range map[string]string{
		"https://orchestrator.example:8443/daemon/linux-amd64": `curl -fsSL --cacert /etc/orchestrator/ca.crt -o /usr/local/bin/orchestrator-daemon.download "https://orchestrator.example:8443/daemon/linux-amd64"`,
		"https://downloads.example/daemon-linux-amd64":         `curl -fsSL -o /usr/local/bin/orchestrator-daemon.download "https://downloads.example/daemon-linux-amd64"`,
		"https://orchestrator.example:8443.evil/daemon":        `curl -fsSL -o /usr/local/bin/orchestrator-daemon.download "https://orchestrator.example:8443.evil/daemon"`,
	} {
		data, err := renderUserData(userDataInput{Token: token, CA: []byte("CA"), ServerURL: "https://orchestrator.example:8443", DaemonURL: url})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(data, want+"\n") {
			t.Errorf("%s: user data lacks %q:\n%s", url, want, data)
		}
	}
}

func TestGivenServerTypeWhenRenderingTheDaemonBinaryURLThenArchIsItsArchitecture(t *testing.T) {
	for _, tt := range []struct {
		serverType, template, want string
	}{
		{"cx23", "https://downloads.example/daemon-{arch}", "https://downloads.example/daemon-amd64"},
		{"cax21", "https://downloads.example/daemon-{arch}", "https://downloads.example/daemon-arm64"},
		{"cx23", "https://downloads.example/daemon", "https://downloads.example/daemon"},
	} {
		if got := daemonBinaryURL(tt.template, tt.serverType); got != tt.want {
			t.Errorf("daemonBinaryURL(%q, %q) = %q, want %q", tt.template, tt.serverType, got, tt.want)
		}
	}
}

func TestGivenURLsACommandLineWouldChangeWhenRenderingUserDataThenTheyAreRefused(t *testing.T) {
	token := protocol.EnrolmentToken{Daemon: "vps-abcdefgh", Secret: "secret"}
	for _, input := range []userDataInput{
		{Token: token, ServerURL: "https://orchestrator.example/%41", DaemonURL: "https://downloads.example/daemon"},
		{Token: token, ServerURL: "https://orchestrator.example/ x", DaemonURL: "https://downloads.example/daemon"},
		{Token: token, ServerURL: "https://orchestrator.example", DaemonURL: "https://downloads.example/$(id)"},
		{Token: token, ServerURL: "https://orchestrator.example", DaemonURL: "https://downloads.example/\"x"},
	} {
		if _, err := renderUserData(input); err == nil {
			t.Errorf("%+v: rendered", input)
		}
	}
}
