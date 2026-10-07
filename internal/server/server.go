package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

const (
	// maxEventBatchBytes bounds one POSTed batch of events. A single
	// harness line can carry a whole file, so the bound is generous.
	maxEventBatchBytes = 32 << 20
	// keepaliveInterval is how often an idle command stream sends a
	// comment, so that proxies and the daemon can tell it is alive.
	keepaliveInterval = 15 * time.Second
)

// Server serves the daemon-facing and owner-facing HTTP APIs over a Store.
type Server struct {
	store *Store
	log   *slog.Logger
	mux   *http.ServeMux
	// defaultModel is the model of a task created without one.
	defaultModel string

	mu      sync.Mutex
	streams map[protocol.DaemonID]*commandStream
	ended   chan struct{}
	endOnce sync.Once

	watchers watchers
}

// commandStream is the one open SSE stream of a daemon.
type commandStream struct {
	// issued holds a signal when a command may have been issued since the
	// stream last read the log.
	issued chan struct{}
	// replaced is closed when another stream for the daemon opens.
	replaced chan struct{}
}

// New returns a server over store. defaultModel is the model of a task
// created without one (docs/adr/2026-10-07-task-interface.md).
func New(store *Store, log *slog.Logger, defaultModel string) *Server {
	s := &Server{
		store:        store,
		log:          log,
		defaultModel: defaultModel,
		mux:          http.NewServeMux(),
		streams:      make(map[protocol.DaemonID]*commandStream),
		ended:        make(chan struct{}),
		watchers:     watchers{byTask: make(map[protocol.TaskID]map[chan struct{}]struct{})},
	}
	s.mux.HandleFunc("POST /v1/daemons/{daemon}/events", s.postEvents)
	s.mux.HandleFunc("GET /v1/daemons/{daemon}/acks", s.getAcks)
	s.mux.HandleFunc("GET /v1/daemons/{daemon}/commands", s.streamCommands)
	s.mux.HandleFunc("GET /v1/tasks", s.getTasks)
	s.mux.HandleFunc("POST /v1/tasks", s.postTask)
	s.mux.HandleFunc("GET /v1/tasks/{task}", s.getTask)
	s.mux.HandleFunc("POST /v1/tasks/{task}/commands", s.postCommand)
	s.mux.HandleFunc("GET /v1/tasks/{task}/events", s.getEvents)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// EndStreams ends every open command stream, and every stream opened
// later once it has sent the commands it owed. Command streams never go
// idle, so http.Server.Shutdown waits for them forever unless this is
// registered with http.Server.RegisterOnShutdown.
func (s *Server) EndStreams() {
	s.endOnce.Do(func() { close(s.ended) })
}

// postEvents stores a daemon's batch of events and acknowledges, per task
// in the batch, the highest seq up to which the server holds every event.
func (s *Server) postEvents(w http.ResponseWriter, r *http.Request) {
	daemon, ok := daemonFromPath(w, r)
	if !ok {
		return
	}
	var events []protocol.Event
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEventBatchBytes)).Decode(&events); err != nil {
		http.Error(w, "decode events: "+err.Error(), http.StatusBadRequest)
		return
	}
	for idx, event := range events {
		if err := validateEvent(event); err != nil {
			http.Error(w, fmt.Sprintf("event %d: %v", idx, err), http.StatusBadRequest)
			return
		}
	}
	held, conflicts, err := s.store.appendEvents(r.Context(), daemon, events)
	if foreign, ok := errors.AsType[*foreignTaskError](err); ok {
		http.Error(w, foreign.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	for task := range held {
		s.taskChanged(task)
	}
	for _, event := range conflicts {
		s.log.Warn("duplicate event differs from the stored one; kept the stored one",
			"daemon", daemon, "task", event.TaskID, "seq", event.Seq)
	}
	writeJSON(w, http.StatusOK, held)
}

func validateEvent(event protocol.Event) error {
	if _, err := protocol.ParseTaskID(string(event.TaskID)); err != nil {
		return err
	}
	if event.Seq == 0 || event.Seq > math.MaxInt64 {
		return fmt.Errorf("seq %d out of range", event.Seq)
	}
	if event.Kind == "" {
		return errors.New("no kind")
	}
	if len(event.Payload) == 0 {
		return errors.New("no payload")
	}
	return nil
}

// getAcks reports the highest seq up to which the server holds every
// event, for each task assigned to the daemon, so that the daemon can
// replay what follows.
func (s *Server) getAcks(w http.ResponseWriter, r *http.Request) {
	daemon, ok := daemonFromPath(w, r)
	if !ok {
		return
	}
	held, err := s.store.heldSeqs(r.Context(), daemon)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, held)
}

// streamCommands sends the daemon its commands as server-sent events, each
// with its command id as the event id: first every command after the
// Last-Event-ID the daemon sent (all of them without one), then each new
// one as it is issued.
func (s *Server) streamCommands(w http.ResponseWriter, r *http.Request) {
	daemon, ok := daemonFromPath(w, r)
	if !ok {
		return
	}
	var after uint64
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		var err error
		if after, err = strconv.ParseUint(raw, 10, 63); err != nil {
			http.Error(w, "Last-Event-ID is not a command id", http.StatusBadRequest)
			return
		}
	}
	if err := s.store.recordSeen(r.Context(), daemon); err != nil {
		s.internalError(w, err)
		return
	}

	// The stream is registered before the log is first read, so a command
	// issued in between is signalled rather than missed.
	stream := s.openStream(daemon)
	defer s.closeStream(daemon, stream)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	sender := http.NewResponseController(w)
	if err := sender.Flush(); err != nil {
		return
	}
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()
	for {
		commands, err := s.store.commandsAfter(r.Context(), daemon, after)
		if err != nil {
			// Ending the stream makes the daemon reconnect from its last id.
			if r.Context().Err() == nil {
				s.log.Error("read command log", "daemon", daemon, "error", err)
			}
			return
		}
		for _, command := range commands {
			data, err := encodeJSON(command)
			if err != nil {
				s.log.Error("encode command", "daemon", daemon, "command", command.ID, "error", err)
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", command.ID, data); err != nil {
				return
			}
			after = command.ID
		}
		if len(commands) > 0 {
			if err := sender.Flush(); err != nil {
				return
			}
		}

	wait:
		for {
			select {
			case <-stream.issued:
				break wait
			case <-keepalive.C:
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
				if err := sender.Flush(); err != nil {
					return
				}
			case <-stream.replaced:
				return
			case <-s.ended:
				return
			case <-r.Context().Done():
				return
			}
		}
	}
}

// openStream makes stream the daemon's one command stream, ending the
// one it replaces.
func (s *Server) openStream(daemon protocol.DaemonID) *commandStream {
	stream := &commandStream{issued: make(chan struct{}, 1), replaced: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.streams[daemon]; ok {
		close(previous.replaced)
	}
	s.streams[daemon] = stream
	return stream
}

func (s *Server) closeStream(daemon protocol.DaemonID, stream *commandStream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams[daemon] == stream {
		delete(s.streams, daemon)
	}
}

// signalIssued wakes the daemon's command stream, if one is open, to read
// the log. It is called after the command is committed.
func (s *Server) signalIssued(daemon protocol.DaemonID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stream, ok := s.streams[daemon]; ok {
		select {
		case stream.issued <- struct{}{}:
		default:
		}
	}
}

// createTask records a task on daemon and issues its start.
func (s *Server) createTask(ctx context.Context, daemon protocol.DaemonID, task protocol.TaskID, start protocol.StartTask) (protocol.Command, error) {
	command, err := s.store.createTask(ctx, daemon, task, start)
	if err != nil {
		return protocol.Command{}, err
	}
	s.signalIssued(daemon)
	s.taskChanged(task)
	return command, nil
}

// issueCommand sends a command to the daemon the task is assigned to.
func (s *Server) issueCommand(ctx context.Context, task protocol.TaskID, kind protocol.CommandKind, payload json.RawMessage) (protocol.Command, error) {
	command, err := s.store.issueCommand(ctx, task, kind, payload)
	if err != nil {
		return protocol.Command{}, err
	}
	s.signalIssued(command.DaemonID)
	s.taskChanged(task)
	return command, nil
}

func daemonFromPath(w http.ResponseWriter, r *http.Request) (protocol.DaemonID, bool) {
	daemon, err := protocol.ParseDaemonID(r.PathValue("daemon"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", false
	}
	return daemon, true
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "error", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// encodeJSON encodes v on one line without escaping HTML characters, so
// that "<", ">" and "&" in stored payloads are not rewritten on the way out.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := encodeJSON(v)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(data, '\n'))
}
