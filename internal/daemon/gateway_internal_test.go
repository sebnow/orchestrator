package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func startTestGateway(t *testing.T) *Gateway {
	t.Helper()
	g, err := StartGateway()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

func connect(t *testing.T, url string) (*mcp.ClientSession, error) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url, MaxRetries: -1}, nil)
	if err == nil {
		t.Cleanup(func() { session.Close() })
	}
	return session, err
}

func mustConnect(t *testing.T, url string) *mcp.ClientSession {
	t.Helper()
	session, err := connect(t, url)
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	return session
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("content = %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content = %T", result.Content[0])
	}
	return text.Text
}

var unusedTask = gatewayTask{
	permission: func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("unexpected permission call")
	},
	acknowledgePause: func(string) (string, error) { return "", errors.New("unexpected acknowledgement") },
}

func TestGivenRegisteredTaskWhenListingToolsThenEveryGatewayToolIsOffered(t *testing.T) {
	g := startTestGateway(t)
	url, _, err := g.register("task-1", nil, unusedTask)
	if err != nil {
		t.Fatal(err)
	}

	tools, err := mustConnect(t, url).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{AcknowledgePauseTool, PermissionTool, SendMessageTool, SpawnTaskTool}) {
		t.Errorf("tools = %q", names)
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") || !strings.HasSuffix(url, "/tasks/task-1/mcp") {
		t.Errorf("url = %q", url)
	}
}

func TestGivenTaskAllowedSomeToolsWhenListingToolsThenOnlyThoseAndThePermissionAndPauseToolsAreOffered(t *testing.T) {
	for _, tc := range []struct {
		allowed []string
		want    []string
	}{
		{[]string{}, []string{AcknowledgePauseTool, PermissionTool}},
		{[]string{SendMessageTool}, []string{AcknowledgePauseTool, PermissionTool, SendMessageTool}},
	} {
		g := startTestGateway(t)
		url, _, err := g.register("task-1", tc.allowed, unusedTask)
		if err != nil {
			t.Fatal(err)
		}

		tools, err := mustConnect(t, url).ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}

		var names []string
		for _, tool := range tools.Tools {
			names = append(names, tool.Name)
		}
		slices.Sort(names)
		if !slices.Equal(names, tc.want) {
			t.Errorf("allowed %q: tools = %q, want %q", tc.allowed, names, tc.want)
		}
		if got, want := gatewayTools(tc.allowed), slices.DeleteFunc(slices.Clone(tc.want), func(tool string) bool { return tool == PermissionTool }); !slices.Equal(got, want) {
			t.Errorf("allowed %q: harness allows %q without asking, want %q", tc.allowed, got, want)
		}
	}
}

func TestGivenAcknowledgePauseCallWhenItArrivesThenTheTaskGetsTheNoteAndTheAgentTheConfirmation(t *testing.T) {
	g := startTestGateway(t)
	notes := make(chan string, 1)
	task := unusedTask
	task.acknowledgePause = func(note string) (string, error) {
		notes <- note
		return "Pause acknowledged.", nil
	}
	url, _, _ := g.register("task-1", nil, task)

	result, err := mustConnect(t, url).CallTool(t.Context(), &mcp.CallToolParams{
		Name:      AcknowledgePauseTool,
		Arguments: map[string]any{"note": "Stopped after step 1; steps 2 and 3 remain."},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := <-notes; got != "Stopped after step 1; steps 2 and 3 remain." {
		t.Errorf("note = %q", got)
	}
	if result.IsError || resultText(t, result) != "Pause acknowledged." {
		t.Errorf("result = %+v", result)
	}
}

func TestGivenAcknowledgePauseWithoutNoteWhenCalledThenTheTaskIsNotTold(t *testing.T) {
	g := startTestGateway(t)
	url, _, _ := g.register("task-1", nil, unusedTask)

	result, err := mustConnect(t, url).CallTool(t.Context(), &mcp.CallToolParams{
		Name:      AcknowledgePauseTool,
		Arguments: map[string]any{},
	})

	if err == nil && !result.IsError {
		t.Errorf("result = %+v, want an error", result)
	}
}

func TestGivenPermissionCallWhenTheTaskAnswersLaterThenTheCallBlocksAndReturnsTheAnswer(t *testing.T) {
	g := startTestGateway(t)
	arguments := make(chan json.RawMessage, 1)
	answer := make(chan string)
	task := unusedTask
	task.permission = func(ctx context.Context, raw json.RawMessage) (string, error) {
		arguments <- raw
		select {
		case text := <-answer:
			return text, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	url, _, _ := g.register("task-1", nil, task)
	session := mustConnect(t, url)

	type callResult struct {
		result *mcp.CallToolResult
		err    error
	}
	done := make(chan callResult, 1)
	go func() {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name:      PermissionTool,
			Arguments: map[string]any{"tool_name": "Bash", "input": map[string]any{"command": "touch a"}},
		})
		done <- callResult{result, err}
	}()

	var got map[string]any
	if err := json.Unmarshal(<-arguments, &got); err != nil {
		t.Fatal(err)
	}
	if got["tool_name"] != "Bash" {
		t.Errorf("arguments = %v", got)
	}
	select {
	case <-done:
		t.Fatal("call returned before the task answered")
	case <-time.After(100 * time.Millisecond):
	}
	answer <- `{"behavior":"allow"}`
	call := <-done
	if call.err != nil {
		t.Fatal(call.err)
	}
	if text := resultText(t, call.result); text != `{"behavior":"allow"}` {
		t.Errorf("reply = %q", text)
	}
}

func TestGivenPermissionHandlerErrorWhenCalledThenTheReplyIsAToolError(t *testing.T) {
	g := startTestGateway(t)
	task := unusedTask
	task.permission = func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("task has ended")
	}
	url, _, _ := g.register("task-1", nil, task)

	result, err := mustConnect(t, url).CallTool(t.Context(), &mcp.CallToolParams{Name: PermissionTool, Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}

	if !result.IsError || resultText(t, result) != "task has ended" {
		t.Errorf("result = %+v", result)
	}
}

func TestGivenTwoTasksWhenEachCallsThenEachCallReachesItsOwnTask(t *testing.T) {
	g := startTestGateway(t)
	acknowledgedBy := make(chan string, 2)
	urls := map[string]string{}
	for _, name := range []string{"task-a", "task-b"} {
		task := unusedTask
		task.acknowledgePause = func(string) (string, error) {
			acknowledgedBy <- name
			return "ok", nil
		}
		url, _, err := g.register(protocol.TaskID(name), nil, task)
		if err != nil {
			t.Fatal(err)
		}
		urls[name] = url
	}

	mustConnect(t, urls["task-b"]).CallTool(t.Context(), &mcp.CallToolParams{Name: AcknowledgePauseTool, Arguments: map[string]any{"note": "n"}})
	mustConnect(t, urls["task-a"]).CallTool(t.Context(), &mcp.CallToolParams{Name: AcknowledgePauseTool, Arguments: map[string]any{"note": "n"}})

	if first, second := <-acknowledgedBy, <-acknowledgedBy; first != "task-b" || second != "task-a" {
		t.Errorf("acknowledged by %s then %s", first, second)
	}
}

func TestGivenUnknownOrUnregisteredTaskWhenConnectingThenTheGatewayRefuses(t *testing.T) {
	g := startTestGateway(t)
	url, unregister, _ := g.register("task-1", nil, unusedTask)
	unregister()

	if _, err := connect(t, url); err == nil {
		t.Error("connected to an unregistered task")
	}
	if _, err := connect(t, strings.Replace(url, "task-1", "task-2", 1)); err == nil {
		t.Error("connected to an unknown task")
	}
}

func TestGivenRegisteredTaskWhenRegisteringItAgainThenError(t *testing.T) {
	g := startTestGateway(t)
	g.register("task-1", nil, unusedTask)

	if _, _, err := g.register("task-1", nil, unusedTask); err == nil {
		t.Error("registered twice")
	}
}

func TestGivenSpawnAndSendCallsWhenTheyArriveThenTheTaskGetsTheArgumentsAndTheAgentTheReplyOrError(t *testing.T) {
	g := startTestGateway(t)
	task := unusedTask
	task.spawnTask = func(_ context.Context, in spawnTaskInput) (string, error) {
		return "spawned " + in.Prompt + " on " + in.Model + " for " + in.Purpose, nil
	}
	task.sendMessage = func(_ context.Context, in sendMessageInput) (string, error) {
		return "", errors.New("refused: task " + in.To + " has ended")
	}
	url, _, _ := g.register("task-1", nil, task)
	session := mustConnect(t, url)

	spawned, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: SpawnTaskTool, Arguments: map[string]any{"purpose": "Check a fruit.", "prompt": "Say PEAR.", "model": "haiku"}})
	if err != nil {
		t.Fatal(err)
	}
	sent, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: SendMessageTool, Arguments: map[string]any{"to": "parent", "text": "PEAR"}})
	if err != nil {
		t.Fatal(err)
	}

	if spawned.IsError || resultText(t, spawned) != "spawned Say PEAR. on haiku for Check a fruit." {
		t.Errorf("spawn result = %+v", spawned)
	}
	if !sent.IsError || resultText(t, sent) != "refused: task parent has ended" {
		t.Errorf("send result = %+v", sent)
	}
}

func TestGivenSpawnTaskWhenListedOrCalledWithoutAPurposeThenItsSchemaRequiresOneAndTheCallIsRefused(t *testing.T) {
	g := startTestGateway(t)
	task := unusedTask
	called := false
	task.spawnTask = func(context.Context, spawnTaskInput) (string, error) {
		called = true
		return "spawned", nil
	}
	url, _, _ := g.register("task-1", nil, task)
	session := mustConnect(t, url)

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	for _, tool := range tools.Tools {
		if tool.Name == SpawnTaskTool {
			raw, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !slices.Contains(schema.Required, "purpose") || !slices.Contains(schema.Required, "prompt") ||
		!strings.Contains(string(schema.Properties["purpose"]), "why the child exists") {
		t.Errorf("spawn_task schema: required %q, purpose %s; want purpose required and described", schema.Required, schema.Properties["purpose"])
	}

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: SpawnTaskTool, Arguments: map[string]any{"prompt": "Say PEAR."}})
	if called || (err == nil && !result.IsError) {
		t.Errorf("a spawn without a purpose reached the daemon: called %t, result %+v, err %v", called, result, err)
	}
}
