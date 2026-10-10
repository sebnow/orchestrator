package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/s3/s3test"
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
	b := newBackups(BackupPolicy{Dir: dir, Every: time.Hour, Keep: 2}, store, steppingClock(start, time.Hour))
	for range 3 {
		attempt := b.backUp(t.Context())
		if attempt.Error != "" || attempt.Size == 0 || attempt.UploadedTo != "" {
			t.Fatalf("attempt %+v", attempt)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if attempt := b.backUp(t.Context()); attempt.Error != "" {
		t.Fatal(attempt.Error)
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
	if _, err := srv.backUpNow(t.Context()); err != errBackupsOff {
		t.Errorf("error %v, want %v", err, errBackupsOff)
	}
}

func TestGivenBucketWhenBackingUpThenTheCopyIsUploadedUnderThePrefixAndTheOldestUploadsBeyondTheCountDeleted(t *testing.T) {
	store, _ := openTestStore(t)
	seedTask(t, store, "d1", "t1")
	bucket := s3test.NewServer(t)
	bucket.PageSize = 1
	untouched := []string{"orchestrator/notes.txt", "orchestrator/old/" + backupName(time.Unix(0, 0)), "elsewhere/" + backupName(time.Unix(0, 0))}
	for _, key := range untouched {
		bucket.SetObject(key, []byte("not a backup of this server"))
	}
	start := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	// An earlier server uploaded its copies plain; they count towards
	// the kept ones, and are deleted as the oldest.
	bucket.SetObject("orchestrator/"+backupName(start.Add(-time.Hour)), []byte("a plain upload"))
	b := newBackups(BackupPolicy{Dir: t.TempDir(), Every: time.Hour, Keep: 2, Bucket: bucket.Client(t), Prefix: "orchestrator"}, store, steppingClock(start, time.Hour))

	var last backupAttempt
	for range 3 {
		if last = b.backUp(t.Context()); last.Error != "" {
			t.Fatal(last.Error)
		}
	}

	newest := "orchestrator/" + backupName(start.Add(2*time.Hour)) + ".gz"
	if last.UploadedTo != "s3://"+s3test.Bucket+"/"+newest {
		t.Errorf("uploaded to %q, want s3://%s/%s", last.UploadedTo, s3test.Bucket, newest)
	}
	objects := bucket.Objects()
	var keys []string
	for key := range objects {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	want := append([]string{"elsewhere/" + backupName(time.Unix(0, 0)), "orchestrator/notes.txt", "orchestrator/old/" + backupName(time.Unix(0, 0))},
		"orchestrator/"+backupName(start.Add(time.Hour))+".gz", newest)
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Errorf("bucket holds %q, want %q", keys, want)
	}
	local, err := os.ReadFile(last.File)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(objects[newest]))
	if err != nil {
		t.Fatalf("the upload is not gzipped: %v", err)
	}
	uploaded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(uploaded, local) {
		t.Errorf("the upload, gunzipped, differs from the local copy")
	}
	if entries, _ := os.ReadDir(filepath.Dir(last.File)); len(entries) != 2 {
		t.Errorf("the backup directory holds %d files, want the two kept copies and no gzipped one", len(entries))
	}
}

func TestGivenFailingBucketWhenBackingUpThenTheLocalCopyIsKeptAndTheFailureRecordedAndShown(t *testing.T) {
	bucket := s3test.NewServer(t)
	bucket.Fail(http.StatusServiceUnavailable)
	dir := t.TempDir()
	srv := startTestServerWith(t, Options{Backups: &BackupPolicy{Dir: dir, Keep: 3, Bucket: bucket.Client(t), Prefix: "orchestrator/"}})

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/backup", "")

	var attempt backupAttempt
	if err := json.Unmarshal([]byte(body), &attempt); err != nil {
		t.Fatalf("status %d %q: %v", status, body, err)
	}
	if status != http.StatusInternalServerError || attempt.File == "" || attempt.UploadedTo != "" || !strings.Contains(attempt.Error, "upload backup") || !strings.Contains(attempt.Error, "503") {
		t.Errorf("status %d, attempt %+v; want 500, a local copy and the upload's failure", status, attempt)
	}
	if strings.Contains(body, s3test.Credentials.SecretKey) {
		t.Errorf("the answer holds the secret key")
	}
	if names, _ := localBackups(dir); len(names) != 1 {
		t.Errorf("local copies %v, want 1", names)
	}
	recorded, err := srv.store.lastBackup(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if recorded == nil || !recorded.At.Equal(attempt.At) || recorded.File != attempt.File || recorded.Error != attempt.Error {
		t.Errorf("recorded %+v, want %+v", recorded, attempt)
	}
	page := send(t, http.MethodGet, srv.url+"/", nil, false).body
	for _, want := range []string{"It failed: ", "upload backup", filepath.Base(attempt.File), "Back up now"} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
	if status, _ := doRequest(t, http.MethodGet, srv.url+"/v1/tasks", ""); status != http.StatusOK {
		t.Errorf("the server stopped serving after the failure: status %d", status)
	}
}

func TestGivenBucketWhenTheOwnerBacksUpFromTheDashboardThenTheLastBackupIsShownUploaded(t *testing.T) {
	bucket := s3test.NewServer(t)
	srv := startTestServerWith(t, Options{Backups: &BackupPolicy{Dir: t.TempDir(), Every: 6 * time.Hour, Keep: 14, Bucket: bucket.Client(t), Prefix: "orchestrator/"}})
	page := send(t, http.MethodGet, srv.url+"/", nil, false).body
	for _, want := range []string{"No backup yet.", "every 6h0m0s", "keeping the newest 14", "s3://" + s3test.Bucket + "/orchestrator/", `action="/backups"`, "Back up now"} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard before a backup lacks %q", want)
		}
	}

	got := send(t, http.MethodPost, srv.url+"/backups", url.Values{}, false)

	if got.status != http.StatusSeeOther || got.header.Get("Location") != "/" {
		t.Fatalf("backup form: status %d, Location %q", got.status, got.header.Get("Location"))
	}
	if len(bucket.Objects()) != 1 {
		t.Fatalf("bucket holds %d objects, want 1", len(bucket.Objects()))
	}
	page = send(t, http.MethodGet, srv.url+"/", nil, false).body
	for _, want := range []string{"Last backup ", "uploaded to ", "s3://" + s3test.Bucket + "/orchestrator/server-"} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard after a backup lacks %q", want)
		}
	}
	if strings.Contains(page, "It failed") {
		t.Errorf("dashboard shows a failure")
	}
}

func TestGivenNoBackupPolicyWhenTheOwnerBacksUpThenBackupsAreOff(t *testing.T) {
	srv := startTestServer(t)
	if status, _ := doRequest(t, http.MethodPost, srv.url+"/v1/backup", ""); status != http.StatusNotFound {
		t.Errorf("backup: status %d, want 404", status)
	}
	if page := send(t, http.MethodGet, srv.url+"/", nil, false).body; !strings.Contains(page, "Backups are off") || strings.Contains(page, "Back up now") {
		t.Errorf("dashboard does not say backups are off")
	}
}

func TestGivenTwoAttemptsWhenRecordedThenOnlyTheLastIsKept(t *testing.T) {
	store, _ := openTestStore(t)
	if last, err := store.lastBackup(t.Context()); err != nil || last != nil {
		t.Fatalf("before any: %+v, %v", last, err)
	}
	first := backupAttempt{At: time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC), Size: 10, File: "/b/1.db", UploadedTo: "s3://b/1.db"}
	second := backupAttempt{At: time.Date(2026, 10, 10, 2, 0, 0, 0, time.UTC), Error: "back up database: disk full"}
	for _, attempt := range []backupAttempt{first, second} {
		if err := store.recordBackup(t.Context(), attempt); err != nil {
			t.Fatal(err)
		}
	}
	last, err := store.lastBackup(t.Context())
	if err != nil || last == nil || !last.At.Equal(second.At) || *last != (backupAttempt{At: last.At, Error: second.Error}) {
		t.Errorf("last %+v, %v; want %+v", last, err, second)
	}
}
