package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/runas"
)

// authStatus is the output of `claude auth status --json`. Claude Code
// 2.1.289 printed loggedIn, authMethod ("claude.ai" logged in, "none"
// logged out), apiProvider, analyticsDisabled, projectsDirectory and
// configDirectory, and, logged in, email, orgId, orgName and
// subscriptionType.
type authStatus struct {
	LoggedIn   *bool  `json:"loggedIn"`
	AuthMethod string `json:"authMethod"`
	Email      string `json:"email"`
	OrgID      string `json:"orgId"`
}

// LoginStatus runs `claude auth status --json` as user. Claude Code
// exits 1 when it is logged out, still printing the status, so the exit
// status is ignored when the output reads as a status.
func (h *Harness) LoginStatus(ctx context.Context, user runas.User) (harness.LoginStatus, error) {
	cmd := user.Command(ctx, "", h.path, []string{"auth", "status", "--json"}, childEnv(os.Environ()), nil)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	status, err := parseAuthStatus(stdout.Bytes())
	if err != nil {
		if runErr != nil {
			return harness.LoginStatus{}, fmt.Errorf("claude auth status: %w: %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return harness.LoginStatus{}, fmt.Errorf("claude auth status: %w", err)
	}
	return status, nil
}

// parseAuthStatus reads the output of `claude auth status --json`. The
// account is the email address and the organisation id, joined by '/':
// usage limits belong to one person's seat in one organisation, and the
// same address may hold seats in several.
func parseAuthStatus(out []byte) (harness.LoginStatus, error) {
	var raw authStatus
	if err := json.Unmarshal(out, &raw); err != nil {
		return harness.LoginStatus{}, fmt.Errorf("read the status: %w", err)
	}
	if raw.LoggedIn == nil {
		return harness.LoginStatus{}, fmt.Errorf("read the status: no loggedIn in %q", out)
	}
	status := harness.LoginStatus{LoggedIn: *raw.LoggedIn, Method: raw.AuthMethod}
	if status.LoggedIn {
		var parts []string
		for _, part := range []string{raw.Email, raw.OrgID} {
			if part != "" {
				parts = append(parts, part)
			}
		}
		status.Account = strings.Join(parts, "/")
	}
	return status, nil
}
