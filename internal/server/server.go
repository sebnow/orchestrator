package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/sebnow/orchestrator/internal/component"
	"github.com/sebnow/orchestrator/internal/pki"
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

// Server serves the daemon-facing and owner-facing HTTP APIs, and the
// owner's GUI, over a Store.
type Server struct {
	store *Store
	log   *slog.Logger
	mux   *http.ServeMux
	// handler is mux behind the cross-origin check.
	handler http.Handler
	// defaultModel is the model of a task created without one.
	defaultModel string
	// insecure serves every route without authentication.
	insecure bool

	mu      sync.Mutex
	streams map[protocol.DaemonID]*commandStream
	ended   chan struct{}
	endOnce sync.Once

	watchers watchers

	sched *scheduler
}

// commandStream is the one open SSE stream of a daemon.
type commandStream struct {
	// issued holds a signal when a command may have been issued since the
	// stream last read the log.
	issued chan struct{}
	// replaced is closed when another stream for the daemon opens.
	replaced chan struct{}
}

// Options configure a Server.
type Options struct {
	// DefaultModel is the model of a task created without one
	// (docs/adr/2026-10-07-task-interface.md).
	DefaultModel string
	// Insecure serves every route without authentication, for a server
	// that listens on loopback without TLS. Otherwise a daemon route
	// needs the daemon's verified client certificate
	// (docs/adr/2026-10-08-daemon-authentication.md), and every route
	// but the login form and the static files needs the owner's token or
	// session (docs/adr/2026-10-08-owner-authentication.md).
	Insecure bool
	// Schedule is what the scheduler admits turns by; the zero value is
	// DefaultSchedulePolicy, and a zero DaemonTimeout is
	// DefaultDaemonTimeout.
	Schedule SchedulePolicy
	// Now is the scheduler's clock, which also stamps when a daemon was
	// last seen; nil is time.Now.
	Now func() time.Time
}

// New returns a server over store. The server hears of store's changes
// from then on, so store must serve no other server.
func New(store *Store, log *slog.Logger, options Options) *Server {
	s := &Server{
		store:        store,
		log:          log,
		defaultModel: options.DefaultModel,
		insecure:     options.Insecure,
		mux:          http.NewServeMux(),
		streams:      make(map[protocol.DaemonID]*commandStream),
		ended:        make(chan struct{}),
		watchers:     watchers{byTask: make(map[protocol.TaskID]map[chan struct{}]struct{})},
	}
	policy := options.Schedule
	if policy == (SchedulePolicy{}) {
		policy = DefaultSchedulePolicy
	}
	if policy.DaemonTimeout == 0 {
		policy.DaemonTimeout = DefaultDaemonTimeout
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	store.now = now
	s.sched = &scheduler{store: store, log: log, policy: policy, now: now, connected: s.connectedDaemons, upSince: now(), wake: make(chan struct{}, 1)}
	store.published = s.storeChanged
	s.mux.Handle("POST /v1/daemons/{daemon}/events", s.daemonOnly(s.postEvents))
	s.mux.Handle("GET /v1/daemons/{daemon}/acks", s.daemonOnly(s.getAcks))
	s.mux.Handle("GET /v1/daemons/{daemon}/commands", s.daemonOnly(s.streamCommands))
	s.mux.Handle("POST /v1/daemons/{daemon}/tasks/{task}/requests", s.daemonOnly(s.postAgentRequest))
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(component.Static)))
	s.routeLogin()

	// Every other route is the owner's.
	owner := http.NewServeMux()
	owner.HandleFunc("GET /v1/tasks", s.getTasks)
	owner.HandleFunc("POST /v1/tasks", s.postTask)
	owner.HandleFunc("GET /v1/tasks/{task}", s.getTask)
	owner.HandleFunc("POST /v1/tasks/{task}/commands", s.postCommand)
	owner.HandleFunc("POST /v1/tasks/{task}/dismiss", s.postDismiss)
	owner.HandleFunc("GET /v1/tasks/{task}/events", s.getEvents)
	s.routeGUI(owner)
	s.mux.Handle("/", s.ownerOnly(owner))

	// Rejects requests other than GET, HEAD and OPTIONS that a browser
	// sent from another origin, so that a page on another site cannot
	// submit the owner's forms, the login form included. Daemons and
	// scripts send neither Sec-Fetch-Site nor Origin, and pass.
	s.handler = http.NewCrossOriginProtection().Handler(s.mux)
	return s
}

// daemonOnly serves next only to the daemon the path names. Unless the
// server is insecure, the request must carry a client certificate that
// the TLS handshake verified, or it gets 401, and the certificate's
// common name must be the path's {daemon}, or it gets 403.
func (s *Server) daemonOnly(next http.HandlerFunc) http.Handler {
	if s.insecure {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			http.Error(w, "a daemon certificate is required", http.StatusUnauthorized)
			return
		}
		daemon, err := pki.DaemonID(r.TLS.VerifiedChains[0][0])
		if err != nil || string(daemon) != r.PathValue("daemon") {
			http.Error(w, fmt.Sprintf("the certificate is not daemon %q's", r.PathValue("daemon")), http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// EndStreams ends every open command stream and task page stream, and
// every stream opened later once it has sent what it owed. Streams never
// go idle, so http.Server.Shutdown waits for them forever unless this is
// registered with http.Server.RegisterOnShutdown.
func (s *Server) EndStreams() {
	s.endOnce.Do(func() { close(s.ended) })
}

// postEvents stores a daemon's batch of events and acknowledges, per task
// in the batch, the highest seq up to which the server holds every event.
// A batch for a task that is not the daemon's is refused with 409 and a
// protocol.EventsRefused saying why.
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
	if moved, ok := errors.AsType[*movedTaskError](err); ok {
		writeJSON(w, http.StatusConflict, protocol.EventsRefused{Reason: protocol.RefusedTaskMoved, TaskID: moved.Task, Message: moved.Error()})
		return
	}
	if foreign, ok := errors.AsType[*foreignTaskError](err); ok {
		writeJSON(w, http.StatusConflict, protocol.EventsRefused{Reason: protocol.RefusedNotAssigned, TaskID: foreign.Task, Message: foreign.Error()})
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
// one it replaces. The daemon is connected while it has one.
func (s *Server) openStream(daemon protocol.DaemonID) *commandStream {
	stream := &commandStream{issued: make(chan struct{}, 1), replaced: make(chan struct{})}
	s.mu.Lock()
	if previous, ok := s.streams[daemon]; ok {
		close(previous.replaced)
	}
	s.streams[daemon] = stream
	s.mu.Unlock()
	s.sched.poke()
	return stream
}

// closeStream ends the daemon's stream and records the daemon seen, since
// it was connected until now.
func (s *Server) closeStream(daemon protocol.DaemonID, stream *commandStream) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.recordSeen(ctx, daemon); err != nil {
		s.log.Error("record the daemon seen as its command stream closes", "daemon", daemon, "error", err)
	}
	s.mu.Lock()
	if s.streams[daemon] == stream {
		delete(s.streams, daemon)
	}
	s.mu.Unlock()
	s.sched.poke()
}

// connectedDaemons returns the daemons with an open command stream.
func (s *Server) connectedDaemons() []protocol.DaemonID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Collect(maps.Keys(s.streams))
}

// Schedule admits queued turns as the scheduling rules allow
// (docs/adr/2026-10-08-scheduling.md) until ctx ends. Turns wait while
// it is not running.
func (s *Server) Schedule(ctx context.Context) {
	s.sched.run(ctx)
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

// storeChanged wakes the command streams of the daemons that fx issued
// commands to, the pages of the tasks it changed, and the scheduler.
func (s *Server) storeChanged(fx effects) {
	if fx.reschedule {
		s.sched.poke()
	}
	for _, command := range fx.issued {
		s.signalIssued(command.DaemonID)
		s.taskChanged(command.TaskID)
	}
	for _, task := range fx.changed {
		s.taskChanged(task)
	}
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
