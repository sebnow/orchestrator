//go:build live

// The live test creates one real server at Hetzner and deletes it, with
// the token in ~/.config/orchestrator/hetzner-token, a file only its
// owner may read; without that file it skips. It spends money: one
// server of the cheaper of cx23 and cax11 in fsn1, for a few minutes.
// Run with:
//
//	go test -tags live -run Live -v ./internal/hetzner/
package hetzner_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/hetzner"
)

func TestLiveGivenTokenWhenAServerIsCreatedThenItRunsAndIsDeletedLeavingNothing(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(home, ".config", "orchestrator", "hetzner-token")
	if _, err := os.Stat(tokenPath); errors.Is(err, fs.ErrNotExist) {
		t.Skipf("%s does not exist; no Hetzner token, so no live test", tokenPath)
	}
	token, err := hetzner.LoadToken(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	serverLifecycle(t, hetzner.NewClient(hetzner.Endpoint, token, nil), 5*time.Second)
}
