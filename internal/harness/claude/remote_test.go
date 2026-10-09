package claude_test

import (
	"encoding/json"
	"testing"

	"github.com/sebnow/orchestrator/internal/harness/claude"
)

func TestGivenPermissionRequestWhenCheckingForARemoteSubagentThenOnlyASubagentToolWithIsolationRemoteIs(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  bool
	}{
		{"Agent with isolation remote", "Agent", `{"description":"Count files","prompt":"Count the files.","isolation":"remote"}`, true},
		{"Task with isolation remote", "Task", `{"prompt":"Count the files.","isolation":"remote"}`, true},
		{"Agent without isolation", "Agent", `{"description":"Count files","prompt":"Count the files."}`, false},
		{"Agent in a worktree", "Agent", `{"prompt":"Count the files.","isolation":"worktree"}`, false},
		{"another tool with an isolation field", "Bash", `{"command":"ls","isolation":"remote"}`, false},
		{"an MCP tool named like the subagent tool", "mcp__orchestrator__Agent", `{"isolation":"remote"}`, false},
		{"isolation that is not a string", "Agent", `{"isolation":true}`, false},
		{"input that is not an object", "Agent", `"remote"`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := claude.IsRemoteSubagent(c.tool, json.RawMessage(c.input)); got != c.want {
				t.Errorf("IsRemoteSubagent(%q, %s) = %v, want %v", c.tool, c.input, got, c.want)
			}
		})
	}
}
