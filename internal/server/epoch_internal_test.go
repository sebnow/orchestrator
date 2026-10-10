package server

import (
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// epochIDs returns the ids of the store's epochs, oldest first, and the
// parent each records.
func epochIDs(t *testing.T, store *Store) (ids, parents []string) {
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

	ids, parents := epochIDs(t, store)

	if len(ids) != 1 || len(ids[0]) != 32 || parents[0] != "" {
		t.Errorf("epochs %q with parents %q, want one of 32 hex digits without a parent", ids, parents)
	}
}

func TestGivenTwoNewDatabasesWhenOpenedThenTheirEpochsDiffer(t *testing.T) {
	first, _ := openTestStore(t)
	second, _ := openTestStore(t)

	a, _ := epochIDs(t, first)
	b, _ := epochIDs(t, second)

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
	commands, err := store.commandsAfter(t.Context(), "laptop", protocol.CommandPosition{})
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := epochIDs(t, store)

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

	ids, parents := epochIDs(t, store)
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

// markRestored writes the marker a restore leaves beside the database at
// path.
func markRestored(t *testing.T, path string) {
	t.Helper()
	if err := MarkRestored(path, "orchestrator/server-2026-10-10T14:30:05Z.db"); err != nil {
		t.Fatal(err)
	}
}

func reopen(t *testing.T, path string) *Store {
	t.Helper()
	store, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestGivenARestoredDatabaseWhenOpenedThenItStartsAnEpochOfItsOwnAndIssuesCommandsUnderIt(t *testing.T) {
	store, path := openTestStore(t)
	seedTask(t, store, "laptop", "task-1")
	backupEpochs, _ := epochIDs(t, store)
	store.Close()
	markRestored(t, path)

	restored := reopen(t, path)

	ids, parents := epochIDs(t, restored)
	if len(ids) != 2 || ids[0] != backupEpochs[0] || parents[1] != backupEpochs[0] {
		t.Fatalf("epochs %q with parents %q, want the backup's and a child of it", ids, parents)
	}
	if _, err := os.Lstat(RestoredMarker(path)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the marker is still there: %v", err)
	}
	login, err := restored.issueDaemonCommand(t.Context(), "laptop", protocol.CommandLogin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if epoch := commandEpoch(t, restored, login.ID); epoch != ids[1] {
		t.Errorf("the command after the restore belongs to epoch %q, want the new %q", epoch, ids[1])
	}
}

// A crash after the new epoch is committed and before the marker is
// removed leaves the marker; the next open starts one more epoch.
func TestGivenTheMarkerLeftAfterTheEpochStartedWhenOpenedAgainThenAnotherEpochFollowsIt(t *testing.T) {
	store, path := openTestStore(t)
	store.Close()
	markRestored(t, path)
	reopen(t, path).Close()
	markRestored(t, path)

	again := reopen(t, path)

	ids, parents := epochIDs(t, again)
	if len(ids) != 3 || parents[1] != ids[0] || parents[2] != ids[1] {
		t.Errorf("epochs %q with parents %q, want a chain of three", ids, parents)
	}
}

func TestGivenNoMarkerWhenOpenedAgainThenTheEpochIsKept(t *testing.T) {
	store, path := openTestStore(t)
	before, _ := epochIDs(t, store)
	store.Close()

	after, _ := epochIDs(t, reopen(t, path))

	if !slices.Equal(before, after) {
		t.Errorf("epochs went from %q to %q, want them kept", before, after)
	}
}
