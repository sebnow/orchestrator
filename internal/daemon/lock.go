package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockRetryInterval is how often lockStateDir retries the lock while
// another daemon holds it.
const lockRetryInterval = 250 * time.Millisecond

// lockStateDir takes an exclusive lock on dir, so that a second daemon
// cannot share its journals, waiting up to wait for another daemon to
// release it; the lock is released when the returned file is closed,
// which happens when the process exits.
func lockStateDir(ctx context.Context, dir string, wait time.Duration, log *slog.Logger) (*os.File, error) {
	path := filepath.Join(dir, "lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock state directory %s: %w", dir, err)
	}
	start := time.Now()
	logged := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("lock state directory %s: %w", dir, err)
		}
		if !logged {
			log.Info("state directory held by another daemon; waiting", "dir", dir, "timeout", wait)
			logged = true
		}
		if time.Since(start) >= wait {
			f.Close()
			return nil, fmt.Errorf("state directory %s is still in use by another daemon after %s", dir, wait)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(lockRetryInterval):
		}
	}
}
