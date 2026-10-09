package server

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// pastedPrompt stands for an instructions file the owner pastes in whole.
const pastedPrompt = "# Senior\n\nYou own the design.\n\n- Decide.\n- Report."

func TestGivenAgentFormWhenSubmittedThenTheAgentIsCreatedAndItsPageShowsItForEditing(t *testing.T) {
	srv := startTestServer(t)

	got := send(t, http.MethodPost, srv.url+"/agents", url.Values{
		"name": {"senior"}, "description": {"Owns the design."}, "system_prompt": {pastedPrompt}, "model": {"sonnet"},
		"tools": {"spawn_task", "send_message"}, "priority": {"high"}, "requires": {"os=linux, gpu=nvidia"},
		"acknowledge": {"30s"}, "cleanup": {"2m"},
	}, false)

	if got.status != http.StatusSeeOther || got.header.Get("Location") != "/agents/senior" {
		t.Fatalf("status = %d to %q (%s), want 303 to the agent's page", got.status, got.header.Get("Location"), got.body)
	}
	a, err := srv.store.agent(t.Context(), "senior")
	want := Agent{
		Name: "senior", Description: "Owns the design.", SystemPrompt: pastedPrompt, Model: "sonnet",
		Tools: []string{"spawn_task", "send_message"}, PauseLimits: &protocol.PauseLimits{Acknowledge: 30 * time.Second, Cleanup: 2 * time.Minute},
		Priority: PriorityHigh, Requires: Labels{"os": "linux", "gpu": "nvidia"},
	}
	if err != nil || !reflect.DeepEqual(a, want) {
		t.Errorf("agent = %+v, %v\nwant %+v", a, err, want)
	}
	page := getPage(t, srv.url+"/agents/senior")
	requireContains(t, page, `<form method="post" action="/agents/senior">`, "\n"+pastedPrompt+"</textarea>",
		`value="spawn_task" checked=""`, `value="gpu=nvidia, os=linux"`, `<option value="high" selected="">`)
	requireContains(t, getPage(t, srv.url+"/agents"), `<a href="/agents/senior">senior</a>`, "Owns the design.")
}

func TestGivenAgentFormWithAProblemWhenSubmittedThenTheFormComesBackWithWhatWasEntered(t *testing.T) {
	srv := startTestServer(t)
	for name, form := range map[string]url.Values{
		"bad requires":       {"name": {"a"}, "system_prompt": {"keep me"}, "requires": {"os"}},
		"one pause limit":    {"name": {"a"}, "system_prompt": {"keep me"}, "acknowledge": {"1m"}},
		"unsafe name":        {"name": {"a b"}, "system_prompt": {"keep me"}},
		"malformed duration": {"name": {"a"}, "system_prompt": {"keep me"}, "acknowledge": {"soon"}, "cleanup": {"1m"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := send(t, http.MethodPost, srv.url+"/agents", form, false)

			if got.status != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422", got.status)
			}
			requireContains(t, got.body, `<p class="problem" role="alert">`, "\nkeep me</textarea>")
		})
	}
	if agents, _ := srv.store.agents(t.Context()); len(agents) != 0 {
		t.Errorf("agents = %+v, want none", agents)
	}
}

func TestGivenAgentPageWhenSavedThenTheAgentIsUpdatedAndWhenDeletedItIsGone(t *testing.T) {
	srv := startTestServer(t)
	if err := srv.store.createAgent(t.Context(), Agent{Name: "junior", Tools: []string{"send_message"}, Priority: PriorityNormal, Requires: Labels{}}); err != nil {
		t.Fatal(err)
	}

	saved := send(t, http.MethodPost, srv.url+"/agents/junior", url.Values{"system_prompt": {"Be careful."}, "priority": {"low"}, "filler": {"on"}}, false)
	a, err := srv.store.agent(t.Context(), "junior")
	if saved.status != http.StatusSeeOther || err != nil || a.SystemPrompt != "Be careful." || a.Priority != PriorityLow || !a.Filler || len(a.Tools) != 0 {
		t.Errorf("save: %d; agent = %+v, %v", saved.status, a, err)
	}
	requireLacks(t, getPage(t, srv.url+"/agents"), "(no prompt)")
	deleted := send(t, http.MethodPost, srv.url+"/agents/junior/delete", url.Values{}, false)
	if deleted.status != http.StatusSeeOther || deleted.header.Get("Location") != "/agents" {
		t.Errorf("delete: %d to %q", deleted.status, deleted.header.Get("Location"))
	}
	if _, err := srv.store.agent(t.Context(), "junior"); err == nil {
		t.Error("the agent is still there")
	}
}
