package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func TestGivenProjectFormWhenSubmittedThenTheProjectIsCreatedAndItsPageShowsItForEditing(t *testing.T) {
	srv := startTestServer(t)
	createAgents(t, srv.store, seniorAgent)

	got := send(t, http.MethodPost, srv.url+"/projects", url.Values{
		"name": {"tools"}, "instructions": {pastedPrompt}, "repo": {"ssh://git@host/tools.git"}, "ref": {"main"}, "default_agent": {"senior"},
	}, false)

	id, found := strings.CutPrefix(got.header.Get("Location"), "/projects/")
	if got.status != http.StatusSeeOther || !found {
		t.Fatalf("status = %d to %q (%s), want 303 to the project's page", got.status, got.header.Get("Location"), got.body)
	}
	p, err := srv.store.project(t.Context(), id)
	if err != nil || p.Name != "tools" || p.Instructions != pastedPrompt || p.Repo != "ssh://git@host/tools.git" || p.Ref != "main" || p.DefaultAgent != "senior" {
		t.Errorf("project = %+v, %v", p, err)
	}
	page := getPage(t, srv.url+"/projects/"+id)
	requireContains(t, page, `<form method="post" action="/projects/`+id+`">`, "\n"+pastedPrompt+"</textarea>",
		`value="ssh://git@host/tools.git"`, `<option value="senior" selected="">senior</option>`)
	requireContains(t, getPage(t, srv.url+"/projects"), `<a href="/projects/`+id+`">tools</a>`, `<code>ssh://git@host/tools.git</code> at <code>main</code>`)
	requireContains(t, getPage(t, srv.url+"/"), `<a href="/projects">Projects</a>`)
}

func TestGivenProjectFormWithAProblemWhenSubmittedThenTheFormComesBackWithWhatWasEntered(t *testing.T) {
	srv := startTestServer(t)
	createProject(t, srv.store, Project{Name: "taken"})
	for name, tc := range map[string]struct {
		form   url.Values
		status int
	}{
		"no name":          {url.Values{"name": {" "}, "instructions": {"keep me"}}, http.StatusUnprocessableEntity},
		"repo without ref": {url.Values{"name": {"a"}, "instructions": {"keep me"}, "repo": {"ssh://host/r.git"}}, http.StatusUnprocessableEntity},
		"unknown agent":    {url.Values{"name": {"a"}, "instructions": {"keep me"}, "default_agent": {"nobody"}}, http.StatusUnprocessableEntity},
		"taken name":       {url.Values{"name": {"taken"}, "instructions": {"keep me"}}, http.StatusConflict},
	} {
		t.Run(name, func(t *testing.T) {
			got := send(t, http.MethodPost, srv.url+"/projects", tc.form, false)

			if got.status != tc.status {
				t.Errorf("status = %d, want %d", got.status, tc.status)
			}
			requireContains(t, got.body, `<p class="problem" role="alert">`, "\nkeep me</textarea>")
		})
	}
	if projects, _ := srv.store.projects(t.Context()); len(projects) != 1 {
		t.Errorf("projects = %+v, want only the first", projects)
	}
}

func TestGivenProjectWithTasksWhenItsPageIsShownThenItListsItsRootTasksAndItsFormStartsATaskInIt(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	// Not filler, so that its task is admitted without a quota reading.
	senior := seniorAgent
	senior.Filler = false
	createAgents(t, srv.store, senior)
	advertise(t, srv.store, "laptop", "sonnet")
	p := createProject(t, srv.store, Project{Name: "tools", Repo: "ssh://git@host/tools.git", Ref: "main", DefaultAgent: "senior"})
	page := getPage(t, srv.url+"/projects/"+p.ID)
	requireContains(t, page, "No tasks in this project yet.",
		`<form method="post" action="/tasks"><input name="project" type="hidden" value="`+p.ID+`">`,
		`<option value="">The project&#39;s default, senior</option><option value="senior" selected="">senior</option>`,
		`<p>Repository: <code>ssh://git@host/tools.git</code> at <code>main</code>, the project&#39;s.</p>`)

	root := queueTaskViaForm(t, srv, "laptop", "Plan the release.", url.Values{"project": {p.ID}, "acknowledge": {""}, "cleanup": {""}})
	queueTaskViaForm(t, srv, "laptop", "Elsewhere.", nil)
	admitTurns(t, srv.store)
	work := &lifecycle{t: t, store: srv.store, task: root}
	work.event(protocol.KindHarnessStarted, started, TaskRunning)
	work.event(protocol.KindBranchPushed, `{"branch":"orchestrator/`+string(root)+`","commit":"eb69b7b37fad09fd0170733cbb1f53dbc1502ae7","ahead":1,"uncommitted":0,"error":""}`, TaskRunning)
	if _, err := srv.store.spawnTask(t.Context(), "laptop", root, "child", protocol.Spawn{Prompt: "Help."}); err != nil {
		t.Fatal(err)
	}

	detail := readTask(t, srv.store, root)
	if detail.Project != p.ID || detail.Agent != "senior" || detail.Start.PauseLimits != *seniorAgent.PauseLimits || detail.Start.Workspace == nil {
		t.Errorf("task = %+v, start %+v; want the project's agent, its pause limits and the project's repository", detail.taskSummary, detail.Start)
	}
	page = getPage(t, srv.url+"/projects/"+p.ID)
	requireContains(t, page, `<tr><td><span class="badge state-running">running</span></td><td><a href="/tasks/`+string(root)+`#task-tree">Plan the release.</a></td>`+
		`<td><a href="/agents/senior">senior</a></td><td>$0.0000</td><td><code>orchestrator/`+string(root)+`</code> at <code>eb69b7b37fad</code></td></tr>`)
	requireLacks(t, page, "Elsewhere.", `href="/tasks/child"`)
	requireContains(t, getPage(t, srv.url+"/tasks/"+string(root)), `<dt>Project</dt><dd><a href="/projects/`+p.ID+`">tools</a></dd>`)
}

func TestGivenProjectsWhenTheDashboardIsShownThenItsFormOffersThemAndAProjectsDefaultAgentGivesItsPauseLimits(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	createAgents(t, srv.store, seniorAgent)
	p := createProject(t, srv.store, Project{Name: "tools", DefaultAgent: "senior"})

	requireContains(t, getPage(t, srv.url+"/"),
		`<select name="project"><option value="" selected="">None</option><option value="`+p.ID+`">tools</option></select>`,
		`<option value="" selected="">None, or the project&#39;s default</option>`)
	task := queueTaskViaForm(t, srv, "laptop", "Plan.", url.Values{"project": {p.ID}, "acknowledge": {""}, "cleanup": {""}})

	if detail := readTask(t, srv.store, task); detail.Project != p.ID || detail.Agent != "senior" || detail.Start.PauseLimits != *seniorAgent.PauseLimits {
		t.Errorf("task = %+v, start %+v; want the project's default agent and its pause limits", detail.taskSummary, detail.Start)
	}
	refused := send(t, http.MethodPost, srv.url+"/tasks", url.Values{"project": {"missing"}, "prompt": {"keep me"}, "daemon": {"laptop"}}, false)
	if refused.status != http.StatusUnprocessableEntity || !strings.Contains(refused.body, "There is no project missing.") {
		t.Errorf("unknown project: %d %s", refused.status, refused.body)
	}
}

func TestGivenProjectPageWhenSavedThenTheProjectIsUpdatedAndWhenDeletedItIsGoneUnlessATaskBelongsToIt(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	used := createProject(t, srv.store, Project{Name: "tools"})
	unused := createProject(t, srv.store, Project{Name: "notes"})
	queueTaskViaForm(t, srv, "laptop", "Plan.", url.Values{"project": {used.ID}})

	saved := send(t, http.MethodPost, srv.url+"/projects/"+unused.ID, url.Values{"name": {"journal"}, "instructions": {"Be terse."}}, false)
	if p, err := srv.store.project(t.Context(), unused.ID); saved.status != http.StatusSeeOther || err != nil || p.Name != "journal" || p.Instructions != "Be terse." {
		t.Errorf("save: %d; project = %+v, %v", saved.status, p, err)
	}
	inUse := send(t, http.MethodPost, srv.url+"/projects/"+used.ID+"/delete", url.Values{}, false)
	if inUse.status != http.StatusConflict || !strings.Contains(inUse.body, "Tasks belong to this project") {
		t.Errorf("delete of a project with a task: %d", inUse.status)
	}
	deleted := send(t, http.MethodPost, srv.url+"/projects/"+unused.ID+"/delete", url.Values{}, false)
	if deleted.status != http.StatusSeeOther || deleted.header.Get("Location") != "/projects" {
		t.Errorf("delete: %d to %q", deleted.status, deleted.header.Get("Location"))
	}
	if missing := send(t, http.MethodGet, srv.url+"/projects/"+unused.ID, nil, false); missing.status != http.StatusNotFound {
		t.Errorf("deleted project's page: %d, want 404", missing.status)
	}
}
