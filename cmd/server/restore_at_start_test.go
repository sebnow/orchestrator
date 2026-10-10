package main

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/s3/s3test"
)

// syncWriter is a stderr that the serving goroutine and the test share.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// serveUntilSignalled runs the server with args until it logs that it
// serves, then sends the process SIGINT and returns the server's status
// and log.
func serveUntilSignalled(t *testing.T, args ...string) (int, string) {
	t.Helper()
	stderr := &syncWriter{}
	served := make(chan int, 1)
	go func() { served <- run(args, io.Discard, stderr) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stderr.String(), "msg=serving") {
		select {
		case status := <-served:
			t.Fatalf("the server exited with %d before serving:\n%s", status, stderr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server did not serve within 10 s:\n%s", stderr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case status := <-served:
		return status, stderr.String()
	case <-time.After(30 * time.Second):
		t.Fatalf("the server did not exit within 30 s of SIGINT:\n%s", stderr)
		return 0, ""
	}
}

// serveArgs are the flags of a loopback server with dbPath, its backups
// in their own directory and bucket's flags.
func serveArgs(t *testing.T, dbPath string, bucket *s3test.Server) []string {
	t.Helper()
	args := []string{"-insecure-loopback", "-listen", "127.0.0.1:0", "-db", dbPath, "-backup-dir", filepath.Join(t.TempDir(), "backups"), "-backup-every", "0"}
	return append(args, bucketFlagsFor(t, bucket)...)
}

func TestGivenAnUploadAndNoDatabaseWhenTheServerStartsThenItRestoresTheNewestWithAnEpochOfItsOwn(t *testing.T) {
	backup := backupWithOwnerToken(t)
	content, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	backupEpochs, _ := epochs(t, backup)
	bucket := s3test.NewServer(t)
	bucket.SetObject("orchestrator/server-2026-10-09T10:00:00Z.db", []byte("an older, plain upload"))
	const newest = "orchestrator/server-2026-10-10T14:30:05Z.db.gz"
	bucket.SetObject(newest, gzipBytes(t, content))
	bucket.SetObject("orchestrator/server-latest.db.gz", []byte("not a backup's name"))
	dbPath := filepath.Join(t.TempDir(), "state", "server.db")

	status, logs := serveUntilSignalled(t, serveArgs(t, dbPath, bucket)...)

	if status != 0 {
		t.Fatalf("status %d, want 0:\n%s", status, logs)
	}
	if !strings.Contains(logs, "restored the database from the bucket") || !strings.Contains(logs, newest) {
		t.Errorf("logs do not report restoring %s:\n%s", newest, logs)
	}
	if !hasOwnerToken(t, dbPath) {
		t.Errorf("the restored database lacks the backup's owner token")
	}
	ids, parents := epochs(t, dbPath)
	if len(ids) != 2 || parents[1] != backupEpochs[0] {
		t.Errorf("epochs %q with parents %q, want the backup's %q and a child of it", ids, parents, backupEpochs)
	}
}

func TestGivenAnEmptyBucketAndNoDatabaseWhenTheServerStartsThenItStartsEmptyAndSaysSo(t *testing.T) {
	bucket := s3test.NewServer(t)
	dbPath := filepath.Join(t.TempDir(), "server.db")

	status, logs := serveUntilSignalled(t, serveArgs(t, dbPath, bucket)...)

	if status != 0 {
		t.Fatalf("status %d, want 0:\n%s", status, logs)
	}
	if !strings.Contains(logs, "no backup in the bucket; starting with an empty database") {
		t.Errorf("logs do not report starting empty:\n%s", logs)
	}
}

func TestGivenADatabaseWhenTheServerStartsWithAnUploadInTheBucketThenTheDatabaseIsKept(t *testing.T) {
	bucket := s3test.NewServer(t)
	bucket.SetObject("orchestrator/server-2026-10-10T14:30:05Z.db.gz", []byte("not even gzip"))
	dbPath := filepath.Join(t.TempDir(), "server.db")
	mustRun(t, "issue-owner-token", "-db", dbPath)

	status, logs := serveUntilSignalled(t, serveArgs(t, dbPath, bucket)...)

	if status != 0 || strings.Contains(logs, "restored") {
		t.Fatalf("status %d, want 0 without a restore:\n%s", status, logs)
	}
	if !hasOwnerToken(t, dbPath) {
		t.Errorf("the database lost its owner token")
	}
}

func TestGivenNoDatabaseWhenTheBucketCannotBeReadOrItsNewestUploadIsDamagedThenTheServerRefusesToStart(t *testing.T) {
	for name, prepare := range map[string]func(*s3test.Server){
		"unreachable": func(bucket *s3test.Server) { bucket.Fail(503) },
		"damaged": func(bucket *s3test.Server) {
			bucket.SetObject("orchestrator/server-2026-10-10T14:30:05Z.db.gz", gzipBytes(t, []byte("not a database")))
		},
	} {
		bucket := s3test.NewServer(t)
		prepare(bucket)
		dbPath := filepath.Join(t.TempDir(), "server.db")

		status, _, stderr := runCommand(serveArgs(t, dbPath, bucket)...)

		if status != 1 || !strings.Contains(stderr, "refusing to start") {
			t.Errorf("%s: status %d, stderr %q; want 1, refusing to start", name, status, stderr)
		}
		if names := dirNames(t, filepath.Dir(dbPath)); len(names) != 0 {
			t.Errorf("%s: the directory holds %v, want nothing", name, names)
		}
	}
}

func TestGivenAWriteAheadLogWithoutItsDatabaseWhenTheServerStartsWithABucketThenItRefusesToRestore(t *testing.T) {
	bucket := s3test.NewServer(t)
	content, err := os.ReadFile(backupWithOwnerToken(t))
	if err != nil {
		t.Fatal(err)
	}
	bucket.SetObject("orchestrator/server-2026-10-10T14:30:05Z.db.gz", gzipBytes(t, content))
	dbPath := filepath.Join(t.TempDir(), "server.db")
	if err := os.WriteFile(dbPath+"-wal", []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	status, _, stderr := runCommand(serveArgs(t, dbPath, bucket)...)

	if status != 1 || !strings.Contains(stderr, "server.db-wal") {
		t.Errorf("status %d, stderr %q; want 1 naming the write-ahead log", status, stderr)
	}
	if _, err := os.Stat(dbPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the database was written: %v", err)
	}
}
