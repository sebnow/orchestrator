package claude

import (
	"encoding/json"
	"slices"
)

// subagentTools are the names Claude Code's subagent tool goes by: the
// tool_use blocks of 2.1.289 call it Agent, its system/init line lists it
// as Task, and the permissions documentation writes rules for Agent.
var subagentTools = []string{"Agent", "Task"}

// remoteIsolation is the subagent tool's isolation value that "launches
// the agent in a remote cloud environment", in the words of the tool's
// schema in Claude Code 2.1.289 (docs/design/2026-10-09-remote-subagents.md).
const remoteIsolation = "remote"

// RemoteSubagentDenial tells the agent why a remote subagent was refused
// and what to do instead.
const RemoteSubagentDenial = `Subagents must run on this machine: isolation "remote" would run this one in a cloud environment. ` +
	`Call the tool again without the isolation parameter, or with isolation "worktree".`

// IsRemoteSubagent reports whether a permission request for tool with
// input would start a subagent in a remote cloud environment rather than
// on the harness's machine.
func IsRemoteSubagent(tool string, input json.RawMessage) bool {
	if !slices.Contains(subagentTools, tool) {
		return false
	}
	var args struct {
		Isolation string `json:"isolation"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return false
	}
	return args.Isolation == remoteIsolation
}

// remoteSubagentSettings returns the --settings value that makes Claude
// Code ask the permission tool before it starts a remote subagent. Under
// --permission-mode default it starts every subagent without asking; an
// ask rule matching the isolation parameter
// (https://code.claude.com/docs/en/permissions.md, "Match by input
// parameter") sends exactly these calls to the gateway, where
// IsRemoteSubagent recognises them.
func remoteSubagentSettings() (string, error) {
	var ask []string
	for _, tool := range subagentTools {
		ask = append(ask, tool+"(isolation:"+remoteIsolation+")")
	}
	settings, err := json.Marshal(map[string]any{"permissions": map[string]any{"ask": ask}})
	return string(settings), err
}
