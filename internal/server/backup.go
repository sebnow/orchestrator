package server

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sebnow/orchestrator/internal/component"
	"github.com/sebnow/orchestrator/internal/html"
	"github.com/sebnow/orchestrator/internal/s3"
)

// Backup writes a consistent copy of the database to path, replacing any
// file there (docs/adr/2026-10-10-sqlite-backups.md). The copy is written
// with VACUUM INTO to a temporary file in path's directory, readable by
// its owner only, and renamed to path once complete, so path never holds
// a partial copy.
func (s *Store) Backup(ctx context.Context, path string) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("back up database: %w", err)
	}
	tempPath := temp.Name()
	// VACUUM INTO writes into an empty file, so the temporary file's mode
	// is the copy's.
	if err := temp.Close(); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("back up database: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, tempPath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("back up database: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("back up database: %w", err)
	}
	return nil
}

// VerifyBackup checks that the database at path, opened read-only,
// passes SQLite's integrity check and has a schema version this server
// can open, and returns that version.
func VerifyBackup(ctx context.Context, path string) (int, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, fmt.Errorf("verify backup: %w", err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}).String())
	if err != nil {
		return 0, fmt.Errorf("verify backup: %w", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return 0, fmt.Errorf("verify backup: %s is not a database: %w", path, err)
	}
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return 0, fmt.Errorf("verify backup: %w", err)
		}
		problems = append(problems, line)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("verify backup: %s is not a database: %w", path, err)
	}
	if len(problems) != 1 || problems[0] != "ok" {
		return 0, fmt.Errorf("verify backup: %s fails the integrity check: %s", path, strings.Join(problems, "; "))
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("verify backup: %s is not the server's database: %w", path, err)
	}
	if version < 1 || version > schemaVersion {
		return 0, fmt.Errorf("verify backup: %s has schema version %d; this server knows versions 1 to %d", path, version, schemaVersion)
	}
	return version, nil
}

// BackupPolicy is where and how often the server backs its database up,
// and how many copies it keeps.
type BackupPolicy struct {
	// Dir holds the copies, named by backupName.
	Dir string
	// Every is the time between scheduled backups; 0 schedules none,
	// leaving those after a harness exits, at shutdown and on demand.
	Every time.Duration
	// Keep is how many copies are kept, at least 1, in Dir and in
	// Bucket each; older ones are deleted after each backup.
	Keep int
	// Bucket, when set, receives a copy of each backup under Prefix.
	Bucket *s3.Client
	// Prefix is the bucket's directory of copies, such as
	// "orchestrator/"; "" is the bucket's top level.
	Prefix string
}

// backupPrefix and backupSuffix surround the UTC time, in RFC 3339, in a
// copy's name. UTC names sort in the order they were taken.
const (
	backupPrefix = "server-"
	backupSuffix = ".db"
)

// backupName names the copy taken at at.
func backupName(at time.Time) string {
	return backupPrefix + at.UTC().Format(time.RFC3339) + backupSuffix
}

// backupTime is when the copy named name was taken; ok is false for a
// name backupName does not make.
func backupTime(name string) (time.Time, bool) {
	stamp, ok := strings.CutPrefix(name, backupPrefix)
	if !ok {
		return time.Time{}, false
	}
	if stamp, ok = strings.CutSuffix(stamp, backupSuffix); !ok {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil || backupName(at) != name {
		return time.Time{}, false
	}
	return at, true
}

// gzipSuffix ends the name of an uploaded copy, which is gzipped
// (docs/adr/2026-10-10-server-loss.md).
const gzipSuffix = ".gz"

// uploadTime is when the upload named name was taken; ok is false for a
// name that is neither a copy's name gzipped, as uploads are named, nor
// a copy's name, as earlier servers named their plain uploads.
func uploadTime(name string) (time.Time, bool) {
	return backupTime(strings.TrimSuffix(name, gzipSuffix))
}

// gzipped writes the file at path, gzipped, to a temporary file beside it
// and returns that file, open at its start, which the caller closes and
// removes.
func gzipped(path string) (*os.File, error) {
	src, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	dst, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+gzipSuffix+".*.tmp")
	if err != nil {
		return nil, err
	}
	zw := gzip.NewWriter(dst)
	_, err = io.Copy(zw, src)
	if err = errors.Join(err, zw.Close()); err == nil {
		_, err = dst.Seek(0, io.SeekStart)
	}
	if err != nil {
		dst.Close()
		os.Remove(dst.Name())
		return nil, err
	}
	return dst, nil
}

// backups writes the server's copies, one at a time.
type backups struct {
	policy BackupPolicy
	store  *Store
	now    func() time.Time
	// remoteDir is what each uploaded copy's key starts with: Prefix
	// with one trailing '/', or "" for the top level.
	remoteDir string
	// mu serializes backups, so that one on demand and one on schedule
	// do not prune each other's copies.
	mu sync.Mutex
	// requests holds a request for a backup made since RunBackups last
	// took one; one is enough, as the next backup covers every write
	// before it.
	requests chan struct{}
}

func newBackups(policy BackupPolicy, store *Store, now func() time.Time) *backups {
	return &backups{policy: policy, store: store, now: now, remoteDir: remoteDir(policy.Prefix), requests: make(chan struct{}, 1)}
}

// remoteDir is what the key of each copy uploaded under prefix starts
// with: prefix with one trailing '/', or "" for the top level.
func remoteDir(prefix string) string {
	if dir := strings.TrimSuffix(prefix, "/"); dir != "" {
		return dir + "/"
	}
	return ""
}

// NewestUpload returns the key of the newest copy in bucket that a
// server with prefix as its BackupPolicy.Prefix uploaded, gzipped or
// plain, and "" when the bucket holds none.
func NewestUpload(ctx context.Context, bucket *s3.Client, prefix string) (string, error) {
	dir := remoteDir(prefix)
	objects, err := bucket.List(ctx, dir+backupPrefix)
	if err != nil {
		return "", fmt.Errorf("list uploaded backups: %w", err)
	}
	var newest string
	var newestAt time.Time
	for _, object := range objects {
		at, ok := uploadTime(strings.TrimPrefix(object.Key, dir))
		if ok && (newest == "" || at.After(newestAt)) {
			newest, newestAt = object.Key, at
		}
	}
	return newest, nil
}

// backupAttempt is the outcome of one backup. File is empty when no copy
// was written, UploadedTo when it was not uploaded, and Error when
// nothing failed.
type backupAttempt struct {
	At         time.Time `json:"at"`
	Size       int64     `json:"size"`
	File       string    `json:"file,omitempty"`
	UploadedTo string    `json:"uploaded_to,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// RemoveBackupLeftovers removes from dir, the backup directory, the
// temporary files that backups cut short by the process ending left
// there: the copy VACUUM INTO was writing and the gzipped copy being
// uploaded. Only a process that makes no backup at the time may call it.
// It returns the paths it removed; a dir that does not exist holds none.
func RemoveBackupLeftovers(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("remove leftover backups: %w", err)
	}
	var removed []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasPrefix(name, "."+backupPrefix) || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			return removed, fmt.Errorf("remove leftover backups: %w", err)
		}
		removed = append(removed, path)
	}
	return removed, nil
}

// localBackups returns the names of the copies in dir, oldest first.
func localBackups(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if _, ok := backupTime(entry.Name()); ok && entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

// backUp writes a copy into the backup directory, uploads it when the
// policy has a bucket, and deletes the oldest copies beyond the policy's
// count in each.
func (b *backups) backUp(ctx context.Context) backupAttempt {
	b.mu.Lock()
	defer b.mu.Unlock()
	attempt := backupAttempt{At: b.now()}
	fail := func(err error) backupAttempt {
		attempt.Error = err.Error()
		return attempt
	}
	if err := os.MkdirAll(b.policy.Dir, 0o700); err != nil {
		return fail(fmt.Errorf("back up database: %w", err))
	}
	path := filepath.Join(b.policy.Dir, backupName(attempt.At))
	if err := b.store.Backup(ctx, path); err != nil {
		return fail(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fail(fmt.Errorf("back up database: %w", err))
	}
	attempt.File, attempt.Size = path, info.Size()
	var errs []error
	names, err := localBackups(b.policy.Dir)
	if err != nil {
		errs = append(errs, fmt.Errorf("prune backups: %w", err))
	}
	for _, name := range names[:max(0, len(names)-b.policy.Keep)] {
		if err := os.Remove(filepath.Join(b.policy.Dir, name)); err != nil {
			errs = append(errs, fmt.Errorf("prune backups: %w", err))
		}
	}
	if b.policy.Bucket != nil {
		uploadedTo, err := b.upload(ctx, path)
		attempt.UploadedTo = uploadedTo
		if err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		attempt.Error = err.Error()
	}
	return attempt
}

// upload puts the copy at path into the bucket, gzipped, and deletes the
// oldest uploaded copies beyond the policy's count, plain uploads of
// earlier servers among them. It returns where the copy went,
// s3://<bucket>/<key>, or "" when it was not uploaded.
func (b *backups) upload(ctx context.Context, path string) (string, error) {
	file, err := gzipped(path)
	if err != nil {
		return "", fmt.Errorf("upload backup: gzip the copy: %w", err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("upload backup: %w", err)
	}
	key := b.remoteDir + filepath.Base(path) + gzipSuffix
	if err := b.policy.Bucket.Put(ctx, key, file, info.Size()); err != nil {
		return "", fmt.Errorf("upload backup: %w", err)
	}
	uploadedTo := "s3://" + b.policy.Bucket.Bucket() + "/" + key
	objects, err := b.policy.Bucket.List(ctx, b.remoteDir+backupPrefix)
	if err != nil {
		return uploadedTo, fmt.Errorf("prune uploaded backups: %w", err)
	}
	var keys []string
	for _, object := range objects {
		if _, ok := uploadTime(strings.TrimPrefix(object.Key, b.remoteDir)); ok {
			keys = append(keys, object.Key)
		}
	}
	slices.Sort(keys)
	var errs []error
	for _, old := range keys[:max(0, len(keys)-b.policy.Keep)] {
		if err := b.policy.Bucket.Delete(ctx, old); err != nil {
			errs = append(errs, fmt.Errorf("prune uploaded backups: %w", err))
		}
	}
	return uploadedTo, errors.Join(errs...)
}

// nextBackup is when the scheduled backup after the newest copy in the
// backup directory is due: now when there is none, so that a server
// restarted more often than the schedule still backs up.
func (b *backups) nextBackup() time.Time {
	names, err := localBackups(b.policy.Dir)
	if err != nil || len(names) == 0 {
		return b.now()
	}
	newest, _ := backupTime(names[len(names)-1])
	return newest.Add(b.policy.Every)
}

// errBackupsOff reports a backup asked of a server started without a
// backup policy.
var errBackupsOff = errors.New("backups are off")

// backUpNow writes a copy of the database now, uploads it when backups
// have a bucket, records the attempt and logs it. A failed attempt is
// recorded with its error, and never stops the server.
func (s *Server) backUpNow(ctx context.Context) (backupAttempt, error) {
	if s.backups == nil {
		return backupAttempt{}, errBackupsOff
	}
	attempt := s.backups.backUp(ctx)
	// The record outlives a backup cut short by the server stopping.
	if err := s.store.recordBackup(context.WithoutCancel(ctx), attempt); err != nil {
		s.log.Error("record backup", "error", err)
	}
	if attempt.Error != "" {
		s.log.Error("back up database", "file", attempt.File, "uploaded_to", attempt.UploadedTo, "error", attempt.Error)
	} else {
		s.log.Info("backed up database", "file", attempt.File, "uploaded_to", attempt.UploadedTo, "bytes", attempt.Size)
	}
	return attempt, nil
}

// recordBackup keeps attempt as the last backup attempt.
func (s *Store) recordBackup(ctx context.Context, attempt backupAttempt) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO backups (id, at, size, file, uploaded_to, error) VALUES (1, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET at = excluded.at, size = excluded.size, file = excluded.file,
			uploaded_to = excluded.uploaded_to, error = excluded.error`,
		formatTime(attempt.At), attempt.Size, attempt.File, attempt.UploadedTo, attempt.Error)
	if err != nil {
		return fmt.Errorf("record backup: %w", err)
	}
	return nil
}

// lastBackup returns the last backup attempt, or nil before the first.
func (s *Store) lastBackup(ctx context.Context) (*backupAttempt, error) {
	var attempt backupAttempt
	var at string
	err := s.db.QueryRowContext(ctx, `SELECT at, size, file, uploaded_to, error FROM backups WHERE id = 1`).
		Scan(&at, &attempt.Size, &attempt.File, &attempt.UploadedTo, &attempt.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read last backup: %w", err)
	}
	if attempt.At, err = parseTime(at); err != nil {
		return nil, fmt.Errorf("read last backup: %w", err)
	}
	return &attempt, nil
}

// requestBackup asks RunBackups for a backup without waiting for it. A
// request made while a backup runs gets one more backup after it,
// however many such requests there are
// (docs/adr/2026-10-10-server-loss.md).
func (s *Server) requestBackup() {
	if s.backups == nil {
		return
	}
	select {
	case s.backups.requests <- struct{}{}:
	default:
	}
}

// RunBackups backs the database up on the policy's schedule and when a
// backup is requested, until ctx ends; without a policy it returns at
// once. The first scheduled backup is due an interval after the newest
// copy, and each later one an interval after the attempt before it,
// whether that succeeded or not and whatever started it.
func (s *Server) RunBackups(ctx context.Context) {
	if s.backups == nil {
		return
	}
	var due <-chan time.Time
	var timer *time.Timer
	if s.backups.policy.Every > 0 {
		timer = time.NewTimer(time.Until(s.backups.nextBackup()))
		defer timer.Stop()
		due = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-due:
		case <-s.backups.requests:
		}
		s.backUpNow(ctx)
		if timer != nil {
			timer.Reset(time.Until(s.backups.now().Add(s.backups.policy.Every)))
		}
	}
}

// BackUpOnShutdown backs the database up once more, for a server that has
// ended its streams and stopped RunBackups, so that the last copy holds
// every write (docs/adr/2026-10-10-server-loss.md). A failure is
// recorded and logged as any backup's is.
func (s *Server) BackUpOnShutdown(ctx context.Context) {
	if s.backups != nil {
		s.backUpNow(ctx)
	}
}

// backupTimeout bounds a backup the owner asked for, upload included.
// It runs to the end even if the owner leaves the page.
const backupTimeout = 30 * time.Minute

// backUpForOwner backs up for an owner's request.
func (s *Server) backUpForOwner(r *http.Request) (backupAttempt, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), backupTimeout)
	defer cancel()
	return s.backUpNow(ctx)
}

// postBackup backs up now and answers with the attempt: 200 when it
// succeeded, 500 when it failed, and 404 when backups are off.
func (s *Server) postBackup(w http.ResponseWriter, r *http.Request) {
	attempt, err := s.backUpForOwner(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	status := http.StatusOK
	if attempt.Error != "" {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, attempt)
}

// postBackupForm backs up now and returns the owner to the dashboard,
// which shows how it went.
func (s *Server) postBackupForm(w http.ResponseWriter, r *http.Request) {
	if _, err := s.backUpForOwner(r); err != nil {
		s.writeHTML(w, http.StatusNotFound, component.FailurePage("Backing up failed", err.Error()+"."))
		return
	}
	redirect(w, r, "/")
}

// backupSection shows the last backup attempt, last nil for none, where
// and how often backups are written, and the button that backs up now.
func (s *Server) backupSection(last *backupAttempt) html.Node {
	if s.backups == nil {
		return component.Backups(nil, "Backups are off: the server was started without a backup directory.", false)
	}
	policy := s.backups.policy
	schedule := "Backups are written after a task's harness exits, at shutdown and on demand"
	if policy.Every > 0 {
		schedule = "Backups are written every " + policy.Every.String() + ", after a task's harness exits, at shutdown and on demand"
	}
	schedule += ", keeping the newest " + strconv.Itoa(policy.Keep) + " in " + policy.Dir
	if policy.Bucket != nil {
		schedule += " and in s3://" + policy.Bucket.Bucket() + "/" + s.backups.remoteDir
	}
	schedule += ". Each holds every task's transcript and the command log."
	var shown *component.Backup
	if last != nil {
		shown = &component.Backup{At: last.At, Size: last.Size, File: last.File, UploadedTo: last.UploadedTo, Error: last.Error}
	}
	return component.Backups(shown, schedule, true)
}
