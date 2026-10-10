package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sebnow/orchestrator/internal/harness/claude"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// Verdict is a permission policy's answer to one permission request.
type Verdict int

const (
	// VerdictAsk hands the request to the owner, who answers it from the
	// task page or the owner API.
	VerdictAsk Verdict = iota
	VerdictAllow
	VerdictDeny
)

// Decision is a Policy's verdict on one permission request. Message tells
// the agent why when the verdict is VerdictDeny.
type Decision struct {
	Verdict Verdict
	Message string
}

// Policy decides permission requests on the server
// (docs/adr/2026-10-08-permission-policy.md). The server asks it once
// per request, as it stores the request, and sends any verdict but
// VerdictAsk to the daemon as the answer_permission command the owner's
// answer would be.
type Policy interface {
	Decide(task protocol.TaskID, request protocol.PermissionRequested) Decision
}

// AllowAll allows every request.
type AllowAll struct{}

func (AllowAll) Decide(protocol.TaskID, protocol.PermissionRequested) Decision {
	return Decision{Verdict: VerdictAllow}
}

// AskOwner hands every request to the owner.
type AskOwner struct{}

func (AskOwner) Decide(protocol.TaskID, protocol.PermissionRequested) Decision {
	return Decision{Verdict: VerdictAsk}
}

// offMachineRule recognises the permission requests of one harness that
// would run the agent's work off the daemon's machine, and says what the
// agent should do instead.
type offMachineRule struct {
	matches func(tool string, input json.RawMessage) bool
	message string
}

// offMachine maps the harness name events carry to its offMachineRule.
var offMachine = map[string]offMachineRule{
	claude.Name: {matches: claude.IsRemoteSubagent, message: claude.RemoteSubagentDenial},
}

// decidePermission answers a request of task, run by the harness named harness. Work
// must not leave the daemon's machine, so a request that would run it
// elsewhere is denied whatever the policy, before policy is asked
// (docs/design/2026-10-09-remote-subagents.md). A nil policy asks the
// owner.
func decidePermission(policy Policy, harness string, task protocol.TaskID, request protocol.PermissionRequested) Decision {
	if rule, ok := offMachine[harness]; ok && rule.matches(request.Tool, request.Input) {
		return Decision{Verdict: VerdictDeny, Message: rule.message}
	}
	if policy == nil {
		return Decision{Verdict: VerdictAsk}
	}
	return policy.Decide(task, request)
}

// answerByPolicy decides each request in requests, which were
// stored in tx and belong to tasks of daemon, and issues the answer of
// each one it decides. A task whose process exited in the same batch is skipped, as
// its process holds no request any more.
func answerByPolicy(ctx context.Context, tx *sql.Tx, policy Policy, daemon protocol.DaemonID, requests []protocol.Event, fx *effects) error {
	for _, event := range requests {
		var request protocol.PermissionRequested
		if err := json.Unmarshal(event.Payload, &request); err != nil || request.RequestID == "" {
			// The owner sees it as an unrecognised record and cannot
			// answer it either.
			continue
		}
		decision := decidePermission(policy, event.Harness.Name, event.TaskID, request)
		if decision.Verdict == VerdictAsk {
			continue
		}
		p, err := loadProgress(ctx, tx, event.TaskID)
		if err != nil {
			return err
		}
		if p.State.Idle() {
			continue
		}
		answer := protocol.AnswerPermission{RequestID: request.RequestID, Allow: decision.Verdict == VerdictAllow}
		if !answer.Allow {
			answer.Message = decision.Message
		}
		payload, err := json.Marshal(answer)
		if err != nil {
			return fmt.Errorf("encode the policy's answer to %s: %w", request.RequestID, err)
		}
		command, err := insertCommand(ctx, tx, daemon, event.TaskID, protocol.CommandAnswerPermission, payload, fx)
		if errors.Is(err, errDismissed) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE commands SET by_policy = 1 WHERE id = ?`, int64(command.ID)); err != nil {
			return fmt.Errorf("record the policy's answer to %s: %w", request.RequestID, err)
		}
	}
	return nil
}
