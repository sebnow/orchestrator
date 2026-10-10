package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
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
	// Keep is how many copies are kept, at least 1; older ones are
	// deleted after each backup.
	Keep int
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
	// mu serializes backups, so that one on demand and one on schedule
	// do not prune each other's copies.
	mu sync.Mutex
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

// backUp writes a copy into the backup directory and deletes the oldest
// copies beyond the policy's count. It returns the copy's path and size.
func (b *backups) backUp(ctx context.Context) (string, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.MkdirAll(b.policy.Dir, 0o700); err != nil {
		return "", 0, fmt.Errorf("back up database: %w", err)
	}
	path := filepath.Join(b.policy.Dir, backupName(b.now()))
	if err := b.store.Backup(ctx, path); err != nil {
		return "", 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, fmt.Errorf("back up database: %w", err)
	}
	names, err := localBackups(b.policy.Dir)
	if err != nil {
		return path, info.Size(), fmt.Errorf("prune backups: %w", err)
	}
	var pruneErr error
	for _, name := range names[:max(0, len(names)-b.policy.Keep)] {
		if err := os.Remove(filepath.Join(b.policy.Dir, name)); err != nil {
			pruneErr = errors.Join(pruneErr, fmt.Errorf("prune backups: %w", err))
		}
	}
	return path, info.Size(), pruneErr
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

// BackUp writes a copy of the database now, and logs the outcome. A
// failure is returned, and never stops the server.
func (s *Server) BackUp(ctx context.Context) (string, error) {
	if s.backups == nil {
		return "", errBackupsOff
	}
	path, size, err := s.backups.backUp(ctx)
	if err != nil {
		s.log.Error("back up database", "path", path, "error", err)
		return path, err
	}
	s.log.Info("backed up database", "path", path, "bytes", size)
	return path, nil
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
		s.BackUp(ctx)
		next = s.backups.now().Add(s.backups.policy.Every)
	}
}
