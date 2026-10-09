package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

const juniorAgent = `{"name":"junior","description":"Does what it is told.","system_prompt":"You are a junior engineer.",` +
	`"model":"haiku","tools":["send_message"],"pause_limits":{"acknowledge":"30s","cleanup":"2m"},"priority":"low","filler":true,"requires":{"os":"linux"}}`

func TestGivenAgentWhenCreatedThenItIsListedAndReadBackAsGiven(t *testing.T) {
	srv := startTestServer(t)

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/agents", juniorAgent)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}

	want := Agent{
		Name: "junior", Description: "Does what it is told.", SystemPrompt: "You are a junior engineer.", Model: "haiku",
		Tools: []string{"send_message"}, PauseLimits: &protocol.PauseLimits{Acknowledge: 30 * time.Second, Cleanup: 2 * time.Minute},
		Priority: PriorityLow, Filler: true, Requires: Labels{"os": "linux"},
	}
	status, body = doRequest(t, http.MethodGet, srv.url+"/v1/agents/junior", "")
	var got Agent
	if status != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("get: %d %s\nwant %+v", status, body, want)
	}
	status, body = doRequest(t, http.MethodGet, srv.url+"/v1/agents", "")
	var list []Agent
	if status != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil || len(list) != 1 || !reflect.DeepEqual(list[0], want) {
		t.Errorf("list: %d %s", status, body)
	}
}

func TestGivenAgentWithOnlyANameWhenCreatedThenItHasNoToolsNormalPriorityAndNoRequirements(t *testing.T) {
	srv := startTestServer(t)

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/agents", `{"name":"bare"}`)

	want := `{"name":"bare","description":"","system_prompt":"","tools":[],"priority":"normal","filler":false,"requires":{}}` + "\n"
	if status != http.StatusCreated || body != want {
		t.Errorf("create: %d %s\nwant %s", status, body, want)
	}
}

func TestGivenInvalidAgentWhenCreatedThenBadRequest(t *testing.T) {
	srv := startTestServer(t)
	for name, body := range map[string]string{
		"no name":          `{"description":"d"}`,
		"unsafe name":      `{"name":"a/b"}`,
		"unknown tool":     `{"name":"a","tools":["Bash"]}`,
		"unknown priority": `{"name":"a","priority":"urgent"}`,
		"zero pause limit": `{"name":"a","pause_limits":{"acknowledge":"0s","cleanup":"1m"}}`,
		"bad label":        `{"name":"a","requires":{"os":"linux mint"}}`,
		"unknown field":    `{"name":"a","role":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if status, response := doRequest(t, http.MethodPost, srv.url+"/v1/agents", body); status != http.StatusBadRequest {
				t.Errorf("status = %d (%s), want 400", status, response)
			}
		})
	}
}

func TestGivenExistingAgentWhenCreatedAgainThenConflictAndTheFirstIsKept(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodPost, srv.url+"/v1/agents", juniorAgent)

	status, _ := doRequest(t, http.MethodPost, srv.url+"/v1/agents", `{"name":"junior"}`)

	if status != http.StatusConflict {
		t.Errorf("status = %d, want 409", status)
	}
	if a, err := srv.store.agent(t.Context(), "junior"); err != nil || a.Model != "haiku" {
		t.Errorf("agent = %+v, %v; want the first kept", a, err)
	}
}

func TestGivenAgentWhenUpdatedThenItIsReplacedButItsNameDoesNotChange(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodPost, srv.url+"/v1/agents", juniorAgent)

	status, body := doRequest(t, http.MethodPut, srv.url+"/v1/agents/junior", `{"system_prompt":"You are careful."}`)

	if status != http.StatusOK {
		t.Fatalf("update: %d %s", status, body)
	}
	a, err := srv.store.agent(t.Context(), "junior")
	if err != nil || a.SystemPrompt != "You are careful." || a.Model != "" || a.PauseLimits != nil || len(a.Tools) != 0 {
		t.Errorf("agent = %+v, %v; want it replaced", a, err)
	}
	if status, _ := doRequest(t, http.MethodPut, srv.url+"/v1/agents/junior", `{"name":"senior"}`); status != http.StatusBadRequest {
		t.Errorf("rename: status = %d, want 400", status)
	}
	if status, _ := doRequest(t, http.MethodPut, srv.url+"/v1/agents/nobody", `{}`); status != http.StatusNotFound {
		t.Errorf("update of an unknown agent: status = %d, want 404", status)
	}
}

func TestGivenAgentWhenDeletedThenItIsGoneUnlessATaskNamesIt(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodPost, srv.url+"/v1/agents", juniorAgent)
	doRequest(t, http.MethodPost, srv.url+"/v1/agents", `{"name":"senior"}`)
	seedTask(t, srv.store, "laptop", "task-1")
	if _, err := srv.store.db.ExecContext(t.Context(), `UPDATE tasks SET agent = 'senior' WHERE id = 'task-1'`); err != nil {
		t.Fatal(err)
	}

	status, _ := doRequest(t, http.MethodDelete, srv.url+"/v1/agents/junior", "")
	inUse, body := doRequest(t, http.MethodDelete, srv.url+"/v1/agents/senior", "")
	unknown, _ := doRequest(t, http.MethodDelete, srv.url+"/v1/agents/junior", "")

	if status != http.StatusNoContent || unknown != http.StatusNotFound {
		t.Errorf("delete: %d, then %d; want 204, then 404", status, unknown)
	}
	if inUse != http.StatusConflict || !strings.Contains(body, "cannot be deleted") {
		t.Errorf("delete of an agent a task names: %d %s, want 409", inUse, body)
	}
}
