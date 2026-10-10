package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// createProject records p, normalised, and returns it as recorded.
func createProject(t *testing.T, store *Store, p Project) Project {
	t.Helper()
	if err := p.normalise(); err != nil {
		t.Fatal(err)
	}
	created, err := store.createProject(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestGivenProjectWhenCreatedThenItIsReadBackAndListedByName(t *testing.T) {
	store, _ := openTestStore(t)
	createAgents(t, store, seniorAgent)

	tools := createProject(t, store, Project{Name: "tools", Instructions: "Use README.md: scopes.", Repo: "ssh://git@host/tools.git", Ref: "main", DefaultAgent: "senior"})
	notes := createProject(t, store, Project{Name: "  notes  "})

	got, err := store.project(t.Context(), tools.ID)
	if err != nil || !reflect.DeepEqual(got, tools) {
		t.Errorf("project = %+v, %v\nwant %+v", got, err, tools)
	}
	if tools.ID == "" || tools.ID == notes.ID || tools.CreatedAt.IsZero() || notes.Name != "notes" {
		t.Errorf("projects = %+v, %+v; want distinct ids, a creation time and a trimmed name", tools, notes)
	}
	list, err := store.projects(t.Context())
	if err != nil || len(list) != 2 || list[0].Name != "notes" || list[1].Name != "tools" {
		t.Errorf("projects = %+v, %v; want notes, then tools", list, err)
	}
}

func TestGivenProjectWhenAnotherTakesItsNameOrNamesAnUnknownAgentThenItIsRefused(t *testing.T) {
	store, _ := openTestStore(t)
	first := createProject(t, store, Project{Name: "tools"})
	other := createProject(t, store, Project{Name: "notes"})

	if _, err := store.createProject(t.Context(), Project{Name: "tools"}); !errors.Is(err, errProjectExists) {
		t.Errorf("create with a taken name: %v, want errProjectExists", err)
	}
	other.Name = "tools"
	if _, err := store.updateProject(t.Context(), other); !errors.Is(err, errProjectExists) {
		t.Errorf("rename to a taken name: %v, want errProjectExists", err)
	}
	first.DefaultAgent = "nobody"
	if _, err := store.updateProject(t.Context(), first); !errors.Is(err, errUnknownAgent) {
		t.Errorf("update naming no agent there is: %v, want errUnknownAgent", err)
	}
	if _, err := store.updateProject(t.Context(), Project{ID: "missing", Name: "x"}); !errors.Is(err, errUnknownProject) {
		t.Errorf("update of an unknown project: %v, want errUnknownProject", err)
	}
}

func TestGivenProjectWhenRenamedThenItKeepsItsIDAndCreationTime(t *testing.T) {
	store, _ := openTestStore(t)
	p := createProject(t, store, Project{Name: "tools", Instructions: "old"})

	p.Name, p.Instructions = "toolbox", "new"
	updated, err := store.updateProject(t.Context(), p)

	got, readErr := store.project(t.Context(), p.ID)
	if err != nil || readErr != nil || got.Name != "toolbox" || got.Instructions != "new" || !got.CreatedAt.Equal(p.CreatedAt) || !reflect.DeepEqual(got, updated) {
		t.Errorf("project = %+v, %v, %v; want it renamed with its creation time", got, err, readErr)
	}
}

func TestGivenProjectWithATaskWhenDeletedThenItIsRefusedAndItsDefaultAgentCannotBeDeleted(t *testing.T) {
	store, _ := openTestStore(t)
	createAgents(t, store, seniorAgent)
	used := createProject(t, store, Project{Name: "tools", DefaultAgent: "senior"})
	unused := createProject(t, store, Project{Name: "notes"})
	task := ownersTask("task-1", "laptop")
	task.Project = used.ID
	queueTask(t, store, task)

	if err := store.deleteProject(t.Context(), used.ID); !errors.Is(err, errProjectInUse) {
		t.Errorf("delete of a project with a task: %v, want errProjectInUse", err)
	}
	if err := store.deleteAgent(t.Context(), "senior"); !errors.Is(err, errAgentInUse) {
		t.Errorf("delete of a project's default agent: %v, want errAgentInUse", err)
	}
	if err := store.deleteProject(t.Context(), unused.ID); err != nil {
		t.Errorf("delete of an unused project: %v", err)
	}
	if err := store.deleteProject(t.Context(), unused.ID); !errors.Is(err, errUnknownProject) {
		t.Errorf("second delete: %v, want errUnknownProject", err)
	}
	if detail := readTask(t, store, "task-1"); detail.Project != used.ID {
		t.Errorf("task project = %q, want %q", detail.Project, used.ID)
	}
}

func TestGivenInvalidProjectWhenNormalisedThenItIsRefused(t *testing.T) {
	for name, p := range map[string]Project{
		"no name":          {Name: "  "},
		"two lines":        {Name: "a\nb"},
		"repo without ref": {Name: "a", Repo: "ssh://host/r.git"},
		"ref without repo": {Name: "a", Ref: "main"},
		"long name":        {Name: strings.Repeat("é", maxProjectNameRunes+1)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := p.normalise(); err == nil {
				t.Errorf("%+v was accepted", p)
			}
		})
	}
}

func TestGivenProjectWhenCreatedThroughTheAPIThenItIsReadListedReplacedAndDeleted(t *testing.T) {
	srv := startTestServer(t)
	createAgents(t, srv.store, seniorAgent)

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/projects",
		`{"name":"tools","instructions":"Scope commits by path.","repo":"ssh://git@host/tools.git","ref":"main","default_agent":"senior"}`)
	var created Project
	if status != http.StatusCreated || json.Unmarshal([]byte(body), &created) != nil || created.ID == "" || created.DefaultAgent != "senior" {
		t.Fatalf("create: %d %s", status, body)
	}
	status, body = doRequest(t, http.MethodGet, srv.url+"/v1/projects/"+created.ID, "")
	var got Project
	if status != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || !reflect.DeepEqual(got, created) {
		t.Errorf("get: %d %s\nwant %+v", status, body, created)
	}
	status, body = doRequest(t, http.MethodGet, srv.url+"/v1/projects", "")
	var list []Project
	if status != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil || len(list) != 1 || list[0].ID != created.ID {
		t.Errorf("list: %d %s", status, body)
	}

	status, body = doRequest(t, http.MethodPut, srv.url+"/v1/projects/"+created.ID, `{"name":"toolbox"}`)
	var updated Project
	if status != http.StatusOK || json.Unmarshal([]byte(body), &updated) != nil || updated.ID != created.ID || updated.Name != "toolbox" ||
		updated.Repo != "" || updated.DefaultAgent != "" || updated.Instructions != "" {
		t.Errorf("replace: %d %s; want it replaced under its id", status, body)
	}

	if status, body := doRequest(t, http.MethodDelete, srv.url+"/v1/projects/"+created.ID, ""); status != http.StatusNoContent {
		t.Errorf("delete: %d %s", status, body)
	}
	if status, _ := doRequest(t, http.MethodGet, srv.url+"/v1/projects/"+created.ID, ""); status != http.StatusNotFound {
		t.Errorf("get after delete: %d, want 404", status)
	}
}

func TestGivenProjectRequestWithAProblemWhenMadeThenItIsRefusedWithItsStatus(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodPost, srv.url+"/v1/projects", `{"name":"tools"}`)
	for name, tc := range map[string]struct {
		method, path, body string
		status             int
	}{
		"no name":               {http.MethodPost, "/v1/projects", `{"instructions":"x"}`, http.StatusBadRequest},
		"repo without ref":      {http.MethodPost, "/v1/projects", `{"name":"a","repo":"ssh://host/r.git"}`, http.StatusBadRequest},
		"id given":              {http.MethodPost, "/v1/projects", `{"id":"mine","name":"a"}`, http.StatusBadRequest},
		"taken name":            {http.MethodPost, "/v1/projects", `{"name":"tools"}`, http.StatusConflict},
		"unknown default agent": {http.MethodPost, "/v1/projects", `{"name":"a","default_agent":"nobody"}`, http.StatusUnprocessableEntity},
		"unknown project":       {http.MethodPut, "/v1/projects/missing", `{"name":"a"}`, http.StatusNotFound},
		"delete unknown":        {http.MethodDelete, "/v1/projects/missing", ``, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			if status, body := doRequest(t, tc.method, srv.url+tc.path, tc.body); status != tc.status {
				t.Errorf("status = %d (%s), want %d", status, body, tc.status)
			}
		})
	}
}

func TestGivenProjectWithARepositoryAndADefaultAgentWhenTheOwnerStartsATaskInItThenTheTaskTakesBoth(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	createAgents(t, srv.store, seniorAgent, Agent{Name: "junior", Tools: []string{}, Priority: PriorityNormal, Requires: Labels{}})
	p := createProject(t, srv.store, Project{Name: "tools", Repo: "ssh://git@host/tools.git", Ref: "main", DefaultAgent: "senior"})

	defaulted := postForTurn(t, srv.url+"/v1/tasks", `{"daemon_id":"laptop","project":"`+p.ID+`","prompt":"Plan."}`, http.StatusCreated)
	named := postForTurn(t, srv.url+"/v1/tasks", `{"daemon_id":"laptop","project":"`+p.ID+`","agent":"junior","prompt":"Do.",`+
		`"workspace":{"repo":"ssh://git@host/tools.git","ref":"main"}}`, http.StatusCreated)

	wantWorkspace := &protocol.Workspace{Repo: "ssh://git@host/tools.git", Ref: "main"}
	for task, agent := range map[protocol.TaskID]string{defaulted.TaskID: "senior", named.TaskID: "junior"} {
		detail := readTask(t, srv.store, task)
		if detail.Project != p.ID || detail.Agent != agent || !reflect.DeepEqual(detail.Start.Workspace, wantWorkspace) {
			t.Errorf("task = %+v, start %+v; want project %s, agent %s and the project's repository", detail.taskSummary, detail.Start, p.ID, agent)
		}
	}
	status, body := doRequest(t, http.MethodGet, srv.url+"/v1/tasks/"+string(defaulted.TaskID), "")
	if status != http.StatusOK || !strings.Contains(body, `"project":"`+p.ID+`"`) {
		t.Errorf("task JSON: %d %s; want its project", status, body)
	}
}

func TestGivenProjectWhenATaskInItAsksForAnotherRepositoryOrTheProjectIsUnknownThenTheTaskIsRefused(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	withRepo := createProject(t, srv.store, Project{Name: "tools", Repo: "ssh://git@host/tools.git", Ref: "main"})
	bare := createProject(t, srv.store, Project{Name: "notes"})

	other, otherBody := doRequest(t, http.MethodPost, srv.url+"/v1/tasks", `{"daemon_id":"laptop","project":"`+withRepo.ID+`","prompt":"p",`+
		`"workspace":{"repo":"ssh://git@host/other.git","ref":"main"},"pause_limits":{"acknowledge":"1m","cleanup":"5m"}}`)
	unknown, unknownBody := doRequest(t, http.MethodPost, srv.url+"/v1/tasks", `{"daemon_id":"laptop","project":"missing","prompt":"p",`+
		`"pause_limits":{"acknowledge":"1m","cleanup":"5m"}}`)
	own := postForTurn(t, srv.url+"/v1/tasks", `{"daemon_id":"laptop","project":"`+bare.ID+`","prompt":"p",`+
		`"workspace":{"repo":"ssh://git@host/other.git","ref":"dev"},"pause_limits":{"acknowledge":"1m","cleanup":"5m"}}`, http.StatusCreated)

	if other != http.StatusBadRequest || !strings.Contains(otherBody, "the project's repository") {
		t.Errorf("another repository: %d %s, want 400", other, otherBody)
	}
	if unknown != http.StatusUnprocessableEntity || !strings.Contains(unknownBody, "unknown project") {
		t.Errorf("unknown project: %d %s, want 422", unknown, unknownBody)
	}
	if detail := readTask(t, srv.store, own.TaskID); detail.Project != bare.ID || detail.Start.Workspace == nil || detail.Start.Workspace.Ref != "dev" {
		t.Errorf("task in a project without a repository = %+v, start %+v; want its own repository", detail.taskSummary, detail.Start)
	}
}

func TestGivenProjectWithInstructionsWhenATaskInItStartsOrSpawnsThenBothSystemPromptsHaveTheInstructionsAfterTheAgentsPrompt(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	createAgents(t, srv.store, Agent{Name: "brain", SystemPrompt: "You coordinate.", Tools: []string{protocol.ToolSpawnTask}, Priority: PriorityNormal, Requires: Labels{}})
	p := createProject(t, srv.store, Project{Name: "tools", Instructions: "Scope commits by path.", DefaultAgent: "brain"})

	turn := postForTurn(t, srv.url+"/v1/tasks", `{"daemon_id":"laptop","project":"`+p.ID+`","prompt":"Plan.","system_prompt":"Be brief."}`, http.StatusCreated)
	admitTurns(t, srv.store)
	(&lifecycle{t: t, store: srv.store, task: turn.TaskID}).event(protocol.KindHarnessStarted, started, TaskRunning)
	if _, err := srv.store.spawnTask(t.Context(), "laptop", turn.TaskID, "child", protocol.Spawn{Purpose: "Help.", Prompt: "Help."}); err != nil {
		t.Fatal(err)
	}

	root, child := readTask(t, srv.store, turn.TaskID), readTask(t, srv.store, "child")
	if !strings.HasSuffix(root.Start.SystemPrompt, "\n\nYou coordinate.\n\nScope commits by path.\n\nBe brief.") {
		t.Errorf("root system prompt = %q; want the agent's, then the project's, then the request's", root.Start.SystemPrompt)
	}
	if child.Project != p.ID || !strings.HasSuffix(child.Start.SystemPrompt, "\n\nScope commits by path.") {
		t.Errorf("child = %+v, system prompt %q; want the project and its instructions", child.taskSummary, child.Start.SystemPrompt)
	}
}
