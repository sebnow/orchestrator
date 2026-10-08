package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// Gateway tool names, as the harness sees them on the gateway.
const (
	PermissionTool       = "permission"
	AcknowledgePauseTool = "acknowledge_pause"
	SpawnTaskTool        = "spawn_task"
	SendMessageTool      = "send_message"
)

// Gateway is the MCP server the daemon hosts for its agents, over
// streamable HTTP on loopback. Each task has its own URL path, so a call
// tells the gateway which task made it.
type Gateway struct {
	server  *http.Server
	baseURL string

	mu    sync.Mutex
	tasks map[protocol.TaskID]*mcp.Server
}

// gatewayTask answers one task's gateway tool calls.
type gatewayTask struct {
	// permission receives the harness's raw permission tool arguments and
	// returns the reply text; it blocks until the request is answered.
	permission func(ctx context.Context, arguments json.RawMessage) (string, error)
	// acknowledgePause receives the agent's stop note and returns the
	// confirmation the agent reads.
	acknowledgePause func(note string) (string, error)
	// spawnTask and sendMessage carry the agent's request to the server
	// and return what the agent is told.
	spawnTask   func(ctx context.Context, in spawnTaskInput) (string, error)
	sendMessage func(ctx context.Context, in sendMessageInput) (string, error)
}

// StartGateway listens on a free loopback port.
func StartGateway() (*Gateway, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("gateway listen: %w", err)
	}
	g := &Gateway{
		baseURL: "http://" + listener.Addr().String(),
		tasks:   map[protocol.TaskID]*mcp.Server{},
	}
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.tasks[protocol.TaskID(r.PathValue("task"))]
	}, nil)
	mux := http.NewServeMux()
	mux.Handle("/tasks/{task}/mcp", handler)
	g.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go g.server.Serve(listener)
	return g, nil
}

// Close stops serving and drops every open session.
func (g *Gateway) Close() error {
	err := g.server.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (g *Gateway) register(task protocol.TaskID, handlers gatewayTask) (url string, unregister func(), err error) {
	server := mcp.NewServer(&mcp.Implementation{Name: "orchestrator", Version: "0.1.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        PermissionTool,
		Description: "Answers the harness's permission prompts on behalf of the operator. Agents do not call it.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		text, err := handlers.permission(ctx, req.Params.Arguments)
		if err != nil {
			return toolError(err), nil
		}
		return toolText(text), nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: AcknowledgePauseTool,
		Description: "Acknowledges a pause request from the operator. Call it only when asked to pause, " +
			"with a short note saying where you stopped and what remains.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in acknowledgePauseInput) (*mcp.CallToolResult, any, error) {
		text, err := handlers.acknowledgePause(in.Note)
		if err != nil {
			return nil, nil, err
		}
		return toolText(text), nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: SpawnTaskTool,
		Description: "Starts a child task: another agent that works on its own on the prompt you give it, " +
			"and reports back to you with " + SendMessageTool + ". Returns the child's task id. " +
			"You are not blocked; to wait for the child, end your turn, and its message arrives as your next prompt.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in spawnTaskInput) (*mcp.CallToolResult, any, error) {
		text, err := handlers.spawnTask(ctx, in)
		if err != nil {
			return toolError(err), nil, nil
		}
		return toolText(text), nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: SendMessageTool,
		Description: "Sends a message to another task by its task id, such as your parent or a child you started. " +
			"The recipient reads it as its next prompt once its current turn has ended.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in sendMessageInput) (*mcp.CallToolResult, any, error) {
		text, err := handlers.sendMessage(ctx, in)
		if err != nil {
			return toolError(err), nil, nil
		}
		return toolText(text), nil, nil
	})

	g.mu.Lock()
	defer g.mu.Unlock()
	if _, taken := g.tasks[task]; taken {
		return "", nil, fmt.Errorf("gateway: task %s already registered", task)
	}
	g.tasks[task] = server
	unregister = func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		delete(g.tasks, task)
	}
	return g.baseURL + "/tasks/" + string(task) + "/mcp", unregister, nil
}

type acknowledgePauseInput struct {
	Note string `json:"note" jsonschema:"where you stopped and what remains, in one or two sentences"`
}

func toolText(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func toolError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}
