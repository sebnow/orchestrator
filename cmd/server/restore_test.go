package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/s3/s3test"
	"github.com/sebnow/orchestrator/internal/server"
)

// backupWithOwnerToken backs up a database with an owner token issued,
// which a database without one lacks, and returns the copy's path.
func backupWithOwnerToken(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	store, err := server.OpenStore(t.Context(), filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.IssueOwnerToken(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "server-2026-10-10T14:30:05Z.db")
	if err := store.Backup(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	return path
}

// hasOwnerToken opens the database at path and reports whether it holds
// an owner token.
func hasOwnerToken(t *testing.T, path string) bool {
	t.Helper()
	store, err := server.OpenStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	has, err := store.HasOwnerToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return has
}

// dirNames lists the names in dir.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestGivenBackupFileWhenRestoredToANewPathThenTheDatabaseIsTheBackupsAndOwnerOnly(t *testing.T) {
	backup := backupWithOwnerToken(t)
	dbPath := filepath.Join(t.TempDir(), "state", "server.db")

	status, stdout, stderr := runCommand("restore", "-db", dbPath, "-from", backup)

	if status != 0 || !strings.Contains(stdout, "schema version") {
		t.Fatalf("status %d, stdout %q, stderr %q", status, stdout, stderr)
	}
	if names := dirNames(t, filepath.Dir(dbPath)); !slices.Equal(names, []string{"server.db"}) {
		t.Errorf("directory holds %v, want only server.db", names)
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode %v, want 0600", mode)
	}
	if !hasOwnerToken(t, dbPath) {
		t.Errorf("the restored database lacks the backup's owner token")
	}
}

func TestGivenExistingDatabaseWhenRestoredThenItIsReplacedOnlyWithForceAndItsWriteAheadLogDropped(t *testing.T) {
	backup := backupWithOwnerToken(t)
	dbPath := filepath.Join(t.TempDir(), "server.db")
	store, err := server.OpenStore(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(dbPath+suffix, []byte("the replaced database's"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	status, _, stderr := runCommand("restore", "-db", dbPath, "-from", backup)
	if status != 1 || !strings.Contains(stderr, "-force") {
		t.Errorf("without -force: status %d, stderr %q; want 1 naming -force", status, stderr)
	}
	if hasOwnerToken(t, dbPath) {
		t.Fatalf("the database was replaced without -force")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(dbPath+suffix, []byte("the replaced database's"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if status, _, stderr := runCommand("restore", "-db", dbPath, "-from", backup, "-force"); status != 0 {
		t.Fatalf("with -force: status %d, stderr %q", status, stderr)
	}
	if names := dirNames(t, filepath.Dir(dbPath)); !slices.Equal(names, []string{"server.db"}) {
		t.Errorf("directory holds %v, want only server.db", names)
	}
	if !hasOwnerToken(t, dbPath) {
		t.Errorf("the restored database lacks the backup's owner token")
	}
}

func TestGivenDamagedBackupWhenRestoredThenItIsRefusedAndTheDatabaseKept(t *testing.T) {
	backup := backupWithOwnerToken(t)
	content, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	damaged := filepath.Join(t.TempDir(), "damaged.db")
	// The header stays, so SQLite opens it; the pages after it are gone.
	if err := os.WriteFile(damaged, content[:len(content)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	notDatabase := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(notDatabase, []byte(strings.Repeat("not a database\n", 400)), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "server.db")
	store, err := server.OpenStore(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	for _, from := range []string{damaged, notDatabase} {
		status, _, stderr := runCommand("restore", "-db", dbPath, "-from", from, "-force")
		if status != 1 || !strings.Contains(stderr, "verify backup") {
			t.Errorf("%s: status %d, stderr %q; want 1 from verifying it", filepath.Base(from), status, stderr)
		}
	}
	if names := dirNames(t, filepath.Dir(dbPath)); !slices.Equal(names, []string{"server.db"}) {
		t.Errorf("directory holds %v, want only server.db", names)
	}
	if hasOwnerToken(t, dbPath) {
		t.Errorf("the database was replaced")
	}
}

func TestGivenBackupKeyInTheBucketWhenRestoredThenItIsDownloaded(t *testing.T) {
	backup := backupWithOwnerToken(t)
	content, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	bucket := s3test.NewServer(t)
	key := "orchestrator/server-2026-10-10T14:30:05Z.db"
	bucket.SetObject(key, content)
	credentials := filepath.Join(t.TempDir(), "backup-s3")
	if err := os.WriteFile(credentials, []byte("access_key="+s3test.Credentials.AccessKey+"\nsecret_key="+s3test.Credentials.SecretKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bucketFlags := []string{"-backup-s3-endpoint", bucket.URL, "-backup-s3-region", s3test.Region,
		"-backup-s3-bucket", s3test.Bucket, "-backup-s3-credentials", credentials}
	dbPath := filepath.Join(t.TempDir(), "server.db")

	status, _, stderr := runCommand(append([]string{"restore", "-db", dbPath, "-from", key}, bucketFlags...)...)

	if status != 0 {
		t.Fatalf("status %d, stderr %q", status, stderr)
	}
	if !hasOwnerToken(t, dbPath) {
		t.Errorf("the restored database lacks the backup's owner token")
	}

	missing := filepath.Join(t.TempDir(), "server.db")
	status, _, stderr = runCommand(append([]string{"restore", "-db", missing, "-from", "orchestrator/missing.db"}, bucketFlags...)...)
	if status != 1 || !strings.Contains(stderr, "NoSuchKey") || strings.Contains(stderr, s3test.Credentials.SecretKey) {
		t.Errorf("missing key: status %d, stderr %q; want 1 naming NoSuchKey", status, stderr)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Errorf("a database was created for a missing key")
	}
}

func TestGivenNoSuchFileAndNoBucketWhenRestoredThenItIsRefused(t *testing.T) {
	status, _, stderr := runCommand("restore", "-db", filepath.Join(t.TempDir(), "server.db"), "-from", "orchestrator/server-2026-10-10T14:30:05Z.db")
	if status != 1 || !strings.Contains(stderr, "no bucket") {
		t.Errorf("status %d, stderr %q; want 1 saying no bucket is given", status, stderr)
	}
}
