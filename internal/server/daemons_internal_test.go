package server

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func daemonNamed(t *testing.T, store *Store, id string) daemonSummary {
	t.Helper()
	daemons, err := store.daemons(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, daemon := range daemons {
		if string(daemon.ID) == id {
			return daemon
		}
	}
	t.Fatalf("no daemon %s among %+v", id, daemons)
	return daemonSummary{}
}

func TestGivenDaemonWhenItReportsFactsThenTheyReplaceThoseItReportedBefore(t *testing.T) {
	srv := startTestServer(t)

	first, _ := doRequest(t, http.MethodPut, srv.url+"/v1/daemons/laptop/facts", `{"os":"darwin","arch":"arm64","gpu":"apple"}`)
	again, _ := doRequest(t, http.MethodPut, srv.url+"/v1/daemons/laptop/facts", `{"os":"darwin","arch":"arm64","cpus":"8"}`)

	if first != http.StatusNoContent || again != http.StatusNoContent {
		t.Fatalf("status = %d, then %d; want 204", first, again)
	}
	if got := daemonNamed(t, srv.store, "laptop").Facts; !reflect.DeepEqual(got, Labels{"os": "darwin", "arch": "arm64", "cpus": "8"}) {
		t.Errorf("facts = %v", got)
	}
}

func TestGivenMalformedFactsWhenReportedThenBadRequest(t *testing.T) {
	srv := startTestServer(t)
	for name, body := range map[string]string{
		"not an object":  `["os"]`,
		"not a string":   `{"cpus":8}`,
		"space in value": `{"gpu":"apple m2"}`,
		"bad key":        `{"o s":"linux"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if status, response := doRequest(t, http.MethodPut, srv.url+"/v1/daemons/laptop/facts", body); status != http.StatusBadRequest {
				t.Errorf("status = %d (%s), want 400", status, response)
			}
		})
	}
}

func TestGivenDaemonPageWhenTheOwnerSetsLabelsThenTheyWinOverFactsWithTheSameKey(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/laptop/facts", `{"os":"linux","gpu":"nvidia"}`)

	got := send(t, http.MethodPost, srv.url+"/daemons/laptop/labels", url.Values{"labels": {"gpu=none, zone=eu"}}, false)

	if got.status != http.StatusSeeOther || got.header.Get("Location") != "/daemons/laptop" {
		t.Fatalf("status = %d to %q (%s)", got.status, got.header.Get("Location"), got.body)
	}
	daemon := daemonNamed(t, srv.store, "laptop")
	if merged := Merge(daemon.Facts, daemon.Labels); !reflect.DeepEqual(merged, Labels{"os": "linux", "gpu": "none", "zone": "eu"}) {
		t.Errorf("merged = %v", merged)
	}
	requireContains(t, getPage(t, srv.url+"/daemons/laptop"), `value="gpu=none, zone=eu"`, "<code>gpu=nvidia</code>", "<code>gpu=none</code>")
	requireContains(t, getPage(t, srv.url+"/"), `<code>gpu=none</code> <code>os=linux</code> <code>zone=eu</code> <a href="/daemons/laptop">edit</a>`)

	refused := send(t, http.MethodPost, srv.url+"/daemons/laptop/labels", url.Values{"labels": {"zone"}}, false)
	if refused.status != http.StatusUnprocessableEntity {
		t.Errorf("malformed labels: status = %d, want 422", refused.status)
	}
	requireContains(t, refused.body, `value="zone"`, `<p class="problem" role="alert">`)
}

func TestGivenDaemonThatSentNoEventsWhenTheDashboardIsShownThenItsHarnessComesFromItsFacts(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/laptop/facts", `{"os":"linux","harness":"claude-code","harness_version":"2.1.289"}`)

	requireContains(t, getPage(t, srv.url+"/"), `<a href="/daemons/laptop">laptop</a></td><td>claude-code 2.1.289</td>`)
}

func TestDaemonHarnessPrefersTheLatestEventsHarnessOverTheFacts(t *testing.T) {
	facts := Labels{"harness": "claude-code", "harness_version": "2.1.289"}
	for name, tc := range map[string]struct {
		daemon daemonSummary
		want   string
	}{
		"event":     {daemonSummary{Harness: &protocol.Harness{Name: "claude-code", Version: "2.1.290"}, Facts: facts}, "claude-code 2.1.290"},
		"facts":     {daemonSummary{Facts: facts}, "claude-code 2.1.289"},
		"name only": {daemonSummary{Facts: Labels{"harness": "claude-code"}}, "claude-code"},
		"neither":   {daemonSummary{Facts: Labels{"os": "linux"}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := daemonHarness(tc.daemon); got != tc.want {
				t.Errorf("daemonHarness = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGivenDaemonThatReportedItsSSHKeyWhenItsPageIsShownThenTheKeysLineIsThereToCopy(t *testing.T) {
	srv := startTestServer(t)
	key := "AAAAC3NzaC1lZDI1NTE5AAAAINUZukIJ+vKsP7bTdxRjE93dbMEuScRvX+q1pD16tdex"
	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/laptop/facts", `{"os":"linux","ssh_public_key":"`+key+`"}`)

	requireContains(t, getPage(t, srv.url+"/daemons/laptop"),
		`<pre class="ssh-key"><code>ssh-ed25519 `+key+` orchestrator@laptop</code></pre>`)
}

func TestGivenDaemonThatReportedNoSSHKeyWhenItsPageIsShownThenItSaysSo(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/laptop/facts", `{"os":"linux"}`)

	requireContains(t, getPage(t, srv.url+"/daemons/laptop"), "The daemon has not reported a key.")
}
