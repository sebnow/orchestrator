package claude

import (
	"maps"
	"slices"
	"strings"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// classTools maps each neutral tool class to Claude Code's built-in
// tools (docs/adr/2026-10-10-agent-models-and-capacity.md). It is the one
// place to change when Claude Code renames or adds a tool: a tool it
// offers that no class names stops every restricted task before its
// first turn. The subagent tool goes by Agent and Task (see
// subagentTools). The mcp class has no built-in tools; it is every tool
// of an MCP server other than the gateway.
var classTools = map[string][]string{
	protocol.ToolClassRead:      {"Read", "Grep", "Glob"},
	protocol.ToolClassEdit:      {"Edit", "Write", "NotebookEdit"},
	protocol.ToolClassShell:     {"Bash"},
	protocol.ToolClassWeb:       {"WebSearch", "WebFetch"},
	protocol.ToolClassSubagents: subagentTools,
	protocol.ToolClassMCP:       nil,
}

// mcpPrefix starts the name of every MCP server's tool, as Claude Code
// lists them: mcp__<server>__<tool>.
const mcpPrefix = "mcp__"

// toolClass returns the class of tool, a name Claude Code lists, or ""
// when no class names it. The gateway's own tools have no class: they
// are always available.
func toolClass(tool string) string {
	if strings.HasPrefix(tool, mcpPrefix) {
		return protocol.ToolClassMCP
	}
	for _, class := range slices.Sorted(maps.Keys(classTools)) {
		if slices.Contains(classTools[class], tool) {
			return class
		}
	}
	return ""
}

// isGatewayTool reports whether tool is one of the gateway's.
func isGatewayTool(tool string) bool {
	return strings.HasPrefix(tool, mcpPrefix+gatewayServer+"__")
}

// allowedTools returns the built-in tools of classes, in class order,
// for --tools, which makes them the only built-in tools Claude Code
// offers. MCP servers' tools are not built in, and --tools leaves them.
func allowedTools(classes []string) []string {
	var tools []string
	for _, class := range protocol.ToolClasses {
		if slices.Contains(classes, class) {
			tools = append(tools, classTools[class]...)
		}
	}
	return tools
}

// checkTools compares tools, those Claude Code lists in system/init, with
// classes, the process's restriction, empty for none.
func checkTools(tools, classes []string) harness.ToolCheck {
	check := harness.ToolCheck{Restricted: len(classes) > 0}
	for _, tool := range tools {
		if isGatewayTool(tool) {
			continue
		}
		switch class := toolClass(tool); {
		case class == "":
			check.Unclassified = append(check.Unclassified, tool)
		case check.Restricted && !slices.Contains(classes, class):
			check.Excess = append(check.Excess, tool)
		}
	}
	return check
}
