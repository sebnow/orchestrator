package server

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// checkCopy opens the database copy at path read-only and fails the test
// unless it passes SQLite's integrity check, is at the current schema
// version and holds task.
func checkCopy(t *testing.T, path string, task protocol.TaskID) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(t.Context(), `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Errorf("integrity_check: %q", integrity)
	}
	var version int
	if err := db.QueryRowContext(t.Context(), `SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Errorf("schema version %d, want %d", version, schemaVersion)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM tasks WHERE id = ?`, task).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("the copy holds %d rows of task %s, want 1", count, task)
	}
}

func TestGivenDatabaseInWALModeWhenBackedUpThenTheCopyIsCompleteConsistentAndOwnerOnly(t *testing.T) {
	store, _ := openTestStore(t)
	seedTask(t, store, "d1", "t1")
	dir := t.TempDir()
	path := filepath.Join(dir, "copy.db")

	if err := store.Backup(t.Context(), path); err != nil {
		t.Fatal(err)
	}

	checkCopy(t, path, "t1")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode %v, want 0600", mode)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"copy.db"}) {
		t.Errorf("directory holds %v, want only copy.db", names)
	}
}

func TestGivenBackupNameWhenParsedThenOnlyNamesBackupNameMakesAreAccepted(t *testing.T) {
	at := time.Date(2026, 10, 10, 14, 30, 5, 0, time.UTC)
	name := backupName(at.In(time.FixedZone("CEST", 2*60*60)))
	if name != "server-2026-10-10T14:30:05Z.db" {
		t.Errorf("name %q", name)
	}
	if got, ok := backupTime(name); !ok || !got.Equal(at) {
		t.Errorf("backupTime(%q) = %v, %v", name, got, ok)
	}
	for _, other := range []string{"server.db", "server-2026-10-10T14:30:05+02:00.db", "server-yesterday.db", ".server-2026-10-10T14:30:05Z.db.123.tmp", "server-2026-10-10T14:30:05Z.db-wal"} {
		if _, ok := backupTime(other); ok {
			t.Errorf("backupTime(%q) accepted it", other)
		}
	}
}

// steppingClock starts at start and moves on by step each time it is
// read.
func steppingClock(start time.Time, step time.Duration) func() time.Time {
	next := start
	return func() time.Time {
		now := next
		next = next.Add(step)
		return now
	}
}

func TestGivenMoreBackupsThanKeptWhenBackingUpThenTheOldestAreDeletedAndOtherFilesKept(t *testing.T) {
	store, _ := openTestStore(t)
	seedTask(t, store, "d1", "t1")
	dir := filepath.Join(t.TempDir(), "backups")
	start := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	b := &backups{policy: BackupPolicy{Dir: dir, Every: time.Hour, Keep: 2}, store: store, now: steppingClock(start, time.Hour)}
	var paths []string
	for range 3 {
		path, size, err := b.backUp(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if size == 0 {
			t.Errorf("%s: size 0", path)
		}
		paths = append(paths, path)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.backUp(t.Context()); err != nil {
		t.Fatal(err)
	}

	names, err := localBackups(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{backupName(start.Add(2 * time.Hour)), backupName(start.Add(3 * time.Hour))}
	if !slices.Equal(names, want) {
		t.Errorf("kept %v, want %v", names, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Errorf("a file other than a backup was touched: %v", err)
	}
	checkCopy(t, filepath.Join(dir, want[1]), "t1")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("backup directory mode %v, want 0700", mode)
	}
}

func TestGivenBackupDirectoryWhenTheNextBackupIsDueThenItIsAnIntervalAfterTheNewestCopyOrNowWithoutOne(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	b := &backups{policy: BackupPolicy{Dir: dir, Every: 6 * time.Hour, Keep: 2}, now: func() time.Time { return now }}
	if got := b.nextBackup(); !got.Equal(now) {
		t.Errorf("without a copy: %v, want %v", got, now)
	}
	for _, at := range []time.Time{now.Add(-5 * time.Hour), now.Add(-2 * time.Hour)} {
		if err := os.WriteFile(filepath.Join(dir, backupName(at)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := b.nextBackup(), now.Add(4*time.Hour); !got.Equal(want) {
		t.Errorf("with copies: %v, want %v", got, want)
	}
}

func TestGivenScheduleAndNoCopyWhenTheServerRunsBackupsThenOneIsWrittenAtOnce(t *testing.T) {
	store, _ := openTestStore(t)
	dir := filepath.Join(t.TempDir(), "backups")
	srv := New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{Backups: &BackupPolicy{Dir: dir, Every: time.Hour, Keep: 3}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.RunBackups(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		names, _ := localBackups(dir)
		if len(names) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backup written; directory holds %v", names)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestGivenServerWithoutBackupPolicyWhenBackingUpThenItIsRefused(t *testing.T) {
	store, _ := openTestStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{})
	if _, err := srv.BackUp(t.Context()); err != errBackupsOff {
		t.Errorf("error %v, want %v", err, errBackupsOff)
	}
}
