package server

import (
	"errors"
	"reflect"
	"strings"
	"testing"
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
