package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// The keys GitHub's meta API listed on 2026-10-10.
const (
	githubEd25519 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	githubECDSA   = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg="
	// forgeKey is a forge's key as ssh-keyscan prints it.
	forgeKey = "git.example.com " + githubEd25519
)

// fakeGitHub serves a meta API answer listing keys, or failing with 503
// while down is set, and counts the requests.
type fakeGitHub struct {
	*httptest.Server
	keys     atomic.Pointer[[]string]
	down     atomic.Bool
	requests atomic.Int64
}

func startFakeGitHub(t *testing.T, keys ...string) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{}
	g.keys.Store(&keys)
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		if g.down.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"verifiable_password_authentication": false, "ssh_keys": *g.keys.Load(), "web": []string{"192.0.2.0/24"}})
	}))
	t.Cleanup(g.Close)
	return g
}

func hostKeysOf(t *testing.T, srv testServer) hostKeysView {
	t.Helper()
	status, body := doRequest(t, http.MethodGet, srv.url+"/v1/host-keys", "")
	var view hostKeysView
	if status != http.StatusOK || json.Unmarshal([]byte(body), &view) != nil {
		t.Fatalf("GET host keys: %d %s", status, body)
	}
	return view
}

func putForgeHostKeys(t *testing.T, srv testServer, text string) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"forge": text})
	if err != nil {
		t.Fatal(err)
	}
	return doRequest(t, http.MethodPut, srv.url+"/v1/host-keys", string(body))
}

func receiveHostKeys(t *testing.T, d *fakeDaemon) []string {
	t.Helper()
	command := receiveCommand(t, d.commands)
	var keys protocol.HostKeys
	if command.Kind != protocol.CommandHostKeys || command.TaskID != "" || json.Unmarshal(command.Payload, &keys) != nil {
		t.Fatalf("command = %+v, want host keys to the daemon itself", command)
	}
	return keys.Lines
}

func TestGivenKnownHostsTextWhenParsedThenEachKeyLineIsKeptAndCommentsAndBlankLinesAreDropped(t *testing.T) {
	text := "# git.example.com:22 SSH-2.0-OpenSSH_9.6\n\n" + forgeKey + "\n  git.example.com\t" + githubECDSA + "  \n@cert-authority *.example.com " + githubEd25519 + " ca\n"

	lines, err := parseHostKeys(text)

	want := []string{forgeKey, "git.example.com " + githubECDSA, "@cert-authority *.example.com " + githubEd25519 + " ca"}
	if err != nil || !slices.Equal(lines, want) {
		t.Errorf("lines = %q, %v; want %q", lines, err, want)
	}
}

func TestGivenLineThatIsNotAHostKeyWhenParsedThenItIsRefusedWithItsLineNumber(t *testing.T) {
	for name, line := range map[string]string{
		"no key":           "git.example.com ssh-ed25519",
		"not base64":       "git.example.com ssh-ed25519 not-base64!",
		"another type":     "git.example.com ssh-rsa " + strings.Fields(githubEd25519)[1],
		"truncated":        "git.example.com ssh-ed25519 AAAAC3Nz",
		"unknown marker":   "@trusted git.example.com " + githubEd25519,
		"marker, no hosts": "@revoked " + githubEd25519,
	} {
		_, err := parseHostKeys(forgeKey + "\n" + line)
		if !errors.Is(err, errInvalidHostKeys) || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("%s: err = %v, want an invalid line 2", name, err)
		}
	}
}

func TestGivenGitHubMetaWhenFetchedThenItsKeysAreKeptForGitHubAndMergedAfterTheOwnersOnce(t *testing.T) {
	github := startFakeGitHub(t, githubEd25519, githubECDSA)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	srv := startTestServerWith(t, Options{GitHubMeta: github.URL, Now: func() time.Time { return now }})
	if status, body := putForgeHostKeys(t, srv, forgeKey+"\ngithub.com "+githubECDSA); status != http.StatusOK {
		t.Fatalf("PUT host keys: %d %s", status, body)
	}

	if err := srv.refreshGitHubHostKeys(t.Context()); err != nil {
		t.Fatal(err)
	}

	view := hostKeysOf(t, srv)
	wantGitHub := []string{"github.com " + githubEd25519, "github.com " + githubECDSA}
	if !slices.Equal(view.GitHub, wantGitHub) || view.GitHubFetchedAt == nil || !view.GitHubFetchedAt.Equal(now) {
		t.Errorf("GitHub's = %q fetched at %v, want %q at %v", view.GitHub, view.GitHubFetchedAt, wantGitHub, now)
	}
	if want := []string{forgeKey, "github.com " + githubECDSA, "github.com " + githubEd25519}; !slices.Equal(view.Lines, want) {
		t.Errorf("lines = %q, want %q", view.Lines, want)
	}
}

func TestGivenGitHubKeysFetchedWhenAFetchFailsOrListsNoKeysThenTheLastOnesStay(t *testing.T) {
	github := startFakeGitHub(t, githubEd25519)
	srv := startTestServerWith(t, Options{GitHubMeta: github.URL})
	if err := srv.refreshGitHubHostKeys(t.Context()); err != nil {
		t.Fatal(err)
	}

	github.down.Store(true)
	failed := srv.refreshGitHubHostKeys(t.Context())
	github.down.Store(false)
	github.keys.Store(&[]string{})
	empty := srv.refreshGitHubHostKeys(t.Context())

	if failed == nil || empty == nil {
		t.Errorf("errors = %v, %v; want both fetches to fail", failed, empty)
	}
	if got := hostKeysOf(t, srv).Lines; !slices.Equal(got, []string{"github.com " + githubEd25519}) {
		t.Errorf("lines = %q, want the first fetch's", got)
	}
}

func TestGivenServerWithAMetaURLWhenItStartsFetchingThenItFetchesAtOnce(t *testing.T) {
	github := startFakeGitHub(t, githubEd25519)
	srv := startTestServerWith(t, Options{GitHubMeta: github.URL})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.FetchGitHubHostKeys(ctx)
	}()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for len(hostKeysOf(t, srv).Lines) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("GitHub's keys not fetched within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if github.requests.Load() != 1 {
		t.Errorf("requests = %d, want 1", github.requests.Load())
	}
}

func TestGivenHostKeysWhenADaemonConnectsThenItGetsThemFirstAndTheLogKeepsOneSetPerDaemon(t *testing.T) {
	srv := startTestServer(t)
	putForgeHostKeys(t, srv, forgeKey)

	first := connectDaemon(t, srv, "vps")
	if got := receiveHostKeys(t, first); !slices.Equal(got, []string{forgeKey}) {
		t.Errorf("lines = %q, want the owner's", got)
	}
	first.vanish()
	again := connectDaemon(t, srv, "vps")
	if got := receiveHostKeys(t, again); !slices.Equal(got, []string{forgeKey}) {
		t.Errorf("lines on reconnecting = %q, want the owner's", got)
	}

	stored, err := srv.store.commandsAfter(t.Context(), "vps", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Kind != protocol.CommandHostKeys {
		t.Errorf("command log = %+v, want the latest host keys alone", stored)
	}
}

func TestGivenNoHostKeysWhenADaemonConnectsThenItGetsNoneUntilItWasSentSome(t *testing.T) {
	srv := startTestServer(t)
	d := connectDaemon(t, srv, "vps")
	if sent, err := srv.store.hasCommandOf(t.Context(), "vps", protocol.CommandHostKeys); err != nil || sent {
		t.Fatalf("host keys sent = %v, %v; want none", sent, err)
	}
	putForgeHostKeys(t, srv, forgeKey)
	receiveHostKeys(t, d)
	putForgeHostKeys(t, srv, "")
	if got := receiveHostKeys(t, d); len(got) != 0 {
		t.Errorf("lines = %q, want none once the owner cleared them", got)
	}
	d.vanish()

	again := connectDaemon(t, srv, "vps")

	if got := receiveHostKeys(t, again); len(got) != 0 {
		t.Errorf("lines on reconnecting = %q, want an empty set, so that a daemon that missed the clearing forgets the keys", got)
	}
}

func TestGivenConnectedDaemonsWhenTheHostKeysChangeThenEachGetsTheUnionAndAnUnchangedSetSendsNothing(t *testing.T) {
	github := startFakeGitHub(t, githubEd25519)
	srv := startTestServerWith(t, Options{GitHubMeta: github.URL})
	a := connectDaemon(t, srv, "a")
	b := connectDaemon(t, srv, "b")

	if err := srv.refreshGitHubHostKeys(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, d := range []*fakeDaemon{a, b} {
		if got := receiveHostKeys(t, d); !slices.Equal(got, []string{"github.com " + githubEd25519}) {
			t.Errorf("daemon %s: lines = %q, want GitHub's", d.id, got)
		}
	}
	if status, body := putForgeHostKeys(t, srv, "# the forge\r\n"+forgeKey+"\r\n"); status != http.StatusOK {
		t.Fatalf("PUT host keys: %d %s", status, body)
	}
	if got := receiveHostKeys(t, a); !slices.Equal(got, []string{forgeKey, "github.com " + githubEd25519}) {
		t.Errorf("lines = %q, want the owner's and GitHub's", got)
	}
	receiveHostKeys(t, b)

	putForgeHostKeys(t, srv, forgeKey)
	if err := srv.refreshGitHubHostKeys(t.Context()); err != nil {
		t.Fatal(err)
	}
	putForgeHostKeys(t, srv, forgeKey+"\n")
	select {
	case event := <-a.commands:
		t.Errorf("command %s, want none for unchanged keys", event.data)
	case <-time.After(200 * time.Millisecond):
	}
	if got := hostKeysOf(t, srv).Forge; got != forgeKey {
		t.Errorf("forge = %q, want the owner's text trimmed", got)
	}
}

func TestGivenInvalidHostKeysWhenPutThenTheyAreRefusedAndTheStoredOnesStay(t *testing.T) {
	srv := startTestServer(t)
	putForgeHostKeys(t, srv, forgeKey)

	status, body := putForgeHostKeys(t, srv, "git.example.com ssh-ed25519 nope")

	if status != http.StatusBadRequest || !strings.Contains(body, "line 1") {
		t.Errorf("PUT: %d %s, want 400 naming line 1", status, body)
	}
	if got := hostKeysOf(t, srv).Forge; got != forgeKey {
		t.Errorf("forge = %q, want the stored keys kept", got)
	}
}

func TestGivenSettingsPageWhenTheOwnerSavesHostKeysThenTheyAreKeptOrRefusedWithTheInputShownAgain(t *testing.T) {
	github := startFakeGitHub(t, githubEd25519)
	srv := startTestServerWith(t, Options{GitHubMeta: github.URL})
	if err := srv.refreshGitHubHostKeys(t.Context()); err != nil {
		t.Fatal(err)
	}

	page := getPage(t, srv.url+"/settings")
	requireContains(t, page, `<a href="/settings">Settings</a>`, `<form method="post" action="/settings/host-keys">`, `<textarea name="forge"`,
		"github.com "+githubEd25519, "Fetched from GitHub")

	saved := send(t, http.MethodPost, srv.url+"/settings/host-keys", url.Values{"forge": {forgeKey}}, false)
	if saved.status != http.StatusSeeOther || saved.header.Get("Location") != "/settings" {
		t.Errorf("save: %d to %q, want 303 to the settings page", saved.status, saved.header.Get("Location"))
	}
	requireContains(t, getPage(t, srv.url+"/settings"), "\n"+forgeKey+"</textarea>")

	refused := send(t, http.MethodPost, srv.url+"/settings/host-keys", url.Values{"forge": {"git.example.com ssh-ed25519"}}, false)
	if refused.status != http.StatusBadRequest {
		t.Errorf("refused: status %d, want 400", refused.status)
	}
	requireContains(t, refused.body, "Not saved: invalid host keys: line 1", "\ngit.example.com ssh-ed25519</textarea>")
	if got := hostKeysOf(t, srv).Forge; got != forgeKey {
		t.Errorf("forge = %q, want the saved keys kept", got)
	}
}

func TestGivenServerThatDoesNotFetchGitHubWhenTheSettingsPageIsShownThenItSaysSo(t *testing.T) {
	srv := startTestServer(t)

	requireContains(t, getPage(t, srv.url+"/settings"), "does not fetch GitHub")
}
