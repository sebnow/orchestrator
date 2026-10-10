package server

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// lineage returns the ids of the store's epochs, oldest first, and the
// parent each records.
func lineage(t *testing.T, store *Store) (ids, parents []string) {
	t.Helper()
	rows, err := store.db.QueryContext(t.Context(), `SELECT id, coalesce(parent, '') FROM epochs ORDER BY ordinal`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent string
		if err := rows.Scan(&id, &parent); err != nil {
			t.Fatal(err)
		}
		ids, parents = append(ids, id), append(parents, parent)
	}
	return ids, parents
}

func commandEpoch(t *testing.T, store *Store, id uint64) string {
	t.Helper()
	var epoch string
	if err := store.db.QueryRowContext(t.Context(), `SELECT epoch FROM commands WHERE id = ?`, int64(id)).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	return epoch
}

func TestGivenANewDatabaseWhenOpenedThenItHasOneEpochWithoutAParent(t *testing.T) {
	store, _ := openTestStore(t)

	ids, parents := lineage(t, store)

	if len(ids) != 1 || len(ids[0]) != 32 || parents[0] != "" {
		t.Errorf("epochs %q with parents %q, want one of 32 hex digits without a parent", ids, parents)
	}
}

func TestGivenTwoNewDatabasesWhenOpenedThenTheirEpochsDiffer(t *testing.T) {
	first, _ := openTestStore(t)
	second, _ := openTestStore(t)

	a, _ := lineage(t, first)
	b, _ := lineage(t, second)

	if a[0] == b[0] {
		t.Errorf("both databases have epoch %s", a[0])
	}
}

func TestGivenAnEpochWhenCommandsAreIssuedThenEachBelongsToIt(t *testing.T) {
	store, _ := openTestStore(t)
	seedTask(t, store, "laptop", "task-1")
	login, err := store.issueDaemonCommand(t.Context(), "laptop", protocol.CommandLogin, nil)
	if err != nil {
		t.Fatal(err)
	}
	commands, err := store.commandsAfter(t.Context(), "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := lineage(t, store)

	for _, command := range commands {
		if epoch := commandEpoch(t, store, command.ID); epoch != ids[0] {
			t.Errorf("command %d belongs to epoch %q, want %q", command.ID, epoch, ids[0])
		}
	}
	if len(commands) != 2 || commands[1].ID != login.ID {
		t.Errorf("commands = %+v, want the start and the login", commands)
	}
}

func TestGivenCommandsBeforeVersion28WhenMigratedThenTheyBelongToTheFirstEpochAndIdsCarryOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.db")
	createVersionOneDatabase(t, path)
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)"}).String())
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO commands (id, daemon_id, task_id, kind, time) VALUES (3, 'laptop', 'old', 'pause', '2026-10-07T10:00:02Z')`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	db.Close()

	store, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ids, parents := lineage(t, store)
	if len(ids) != 1 || parents[0] != "" {
		t.Fatalf("epochs %q with parents %q, want one without a parent", ids, parents)
	}
	if epoch := commandEpoch(t, store, 3); epoch != ids[0] {
		t.Errorf("command 3 belongs to epoch %q, want the first, %q", epoch, ids[0])
	}
	login, err := store.issueDaemonCommand(t.Context(), "laptop", protocol.CommandLogin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if login.ID != 4 {
		t.Errorf("the next command's id = %d, want 4", login.ID)
	}
	rows, err := store.db.QueryContext(t.Context(), `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Error("the migrated database has foreign key violations")
	}
}
