package daemon

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestGivenAStateDirectoryLockedByOneDaemonWhenAnotherLocksItThenItIsRefused(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	log := testLogger(t)

	first, err := lockStateDir(ctx, dir, time.Second, log)
	if err != nil {
		t.Fatalf("first lockStateDir: %v", err)
	}

	_, err = lockStateDir(ctx, dir, 200*time.Millisecond, log)
	if err == nil {
		t.Fatal("second lockStateDir: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "still in use") || !strings.Contains(err.Error(), "after") {
		t.Fatalf("second lockStateDir error = %q; want it to say the directory is still in use after the wait", err.Error())
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close the first lock: %v", err)
	}

	third, err := lockStateDir(ctx, dir, time.Second, log)
	if err != nil {
		t.Fatalf("third lockStateDir, after the first lock closed: %v", err)
	}
	third.Close()
}

func TestGivenAStateDirectoryLockedByOneDaemonWhenItIsReleasedDuringTheWaitThenTheSecondAcquiresIt(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	log := testLogger(t)

	first, err := lockStateDir(ctx, dir, time.Second, log)
	if err != nil {
		t.Fatalf("first lockStateDir: %v", err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		first.Close()
	}()

	second, err := lockStateDir(ctx, dir, 2*time.Second, log)
	if err != nil {
		t.Fatalf("second lockStateDir: want it to acquire the lock once the first releases it, got: %v", err)
	}
	second.Close()
}
