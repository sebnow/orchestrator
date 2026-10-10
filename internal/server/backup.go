package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
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

// BackupPolicy is where and how often the server backs its database up,
// and how many copies it keeps.
type BackupPolicy struct {
	// Dir holds the copies, named by backupName.
	Dir string
	// Every is the time between scheduled backups; 0 backs up only on
	// demand.
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
}

func newBackups(policy BackupPolicy, store *Store, now func() time.Time) *backups {
	b := &backups{policy: policy, store: store, now: now}
	if dir := strings.TrimSuffix(policy.Prefix, "/"); dir != "" {
		b.remoteDir = dir + "/"
	}
	return b
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
		uploadedTo, err := b.upload(ctx, path, attempt.Size)
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

// upload puts the copy at path, of size bytes, into the bucket and
// deletes the oldest uploaded copies beyond the policy's count. It
// returns where the copy went, s3://<bucket>/<key>, or "" when it was
// not uploaded.
func (b *backups) upload(ctx context.Context, path string, size int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("upload backup: %w", err)
	}
	defer file.Close()
	key := b.remoteDir + filepath.Base(path)
	if err := b.policy.Bucket.Put(ctx, key, file, size); err != nil {
		return "", fmt.Errorf("upload backup: %w", err)
	}
	uploadedTo := "s3://" + b.policy.Bucket.Bucket() + "/" + key
	objects, err := b.policy.Bucket.List(ctx, b.remoteDir+backupPrefix)
	if err != nil {
		return uploadedTo, fmt.Errorf("prune uploaded backups: %w", err)
	}
	var keys []string
	for _, object := range objects {
		if _, ok := backupTime(strings.TrimPrefix(object.Key, b.remoteDir)); ok {
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

// RunBackups backs the database up on the policy's schedule until ctx
// ends; with no schedule it returns at once. The first backup is due an
// interval after the newest copy, and each later one an interval after
// the attempt before it, whether that succeeded or not.
func (s *Server) RunBackups(ctx context.Context) {
	if s.backups == nil || s.backups.policy.Every <= 0 {
		return
	}
	next := s.backups.nextBackup()
	for {
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.backUpNow(ctx)
		next = s.backups.now().Add(s.backups.policy.Every)
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
	schedule := "Backups are written on demand only"
	if policy.Every > 0 {
		schedule = "Backups are written every " + policy.Every.String()
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
