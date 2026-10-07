package server

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/sebnow/orchestrator/internal/harness/claude"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// normaliser turns the payload of one harness_output event into
// transcript bodies.
type normaliser func(payload json.RawMessage) []transcript.Body

// normalisers maps the harness name events carry to the normaliser of
// that harness's output.
var normalisers = map[string]normaliser{
	claude.Name: claude.Normalise,
}

// Transcript returns the readable history of task, derived from its
// stored events and commands.
func (s *Server) Transcript(ctx context.Context, task protocol.TaskID) ([]transcript.Entry, error) {
	events, commands, err := s.store.taskHistory(ctx, task)
	if err != nil {
		return nil, err
	}
	return assemble(events, commands), nil
}

// assemble merges a task's events, in seq order, with its commands, in id
// order, into one transcript. Each list keeps its own order; where they
// interleave is decided by time, an event first when the times are equal.
// The times come from two clocks, the daemon's and the server's, so the
// interleaving is only as good as their agreement.
func assemble(events []protocol.Event, commands []protocol.Command) []transcript.Entry {
	entries := make([]transcript.Entry, 0, len(events)+len(commands))
	for len(events) > 0 || len(commands) > 0 {
		if len(commands) == 0 || len(events) > 0 && !commands[0].Time.Before(events[0].Time) {
			event := events[0]
			events = events[1:]
			source := transcript.Source{TaskID: event.TaskID, Seq: event.Seq}
			for _, body := range eventBodies(event) {
				entries = append(entries, transcript.Entry{Time: event.Time, Source: source, Body: body})
			}
			continue
		}
		command := commands[0]
		commands = commands[1:]
		entries = append(entries, transcript.Entry{
			Time:   command.Time,
			Source: transcript.Source{TaskID: command.TaskID, CommandID: command.ID},
			Body:   commandBody(command),
		})
	}
	return entries
}

// eventBodies describes one event. A harness line goes to the normaliser
// of the harness that wrote it.
func eventBodies(event protocol.Event) []transcript.Body {
	if event.Kind == protocol.KindHarnessOutput {
		if normalise, ok := normalisers[event.Harness.Name]; ok {
			return normalise(event.Payload)
		}
		return []transcript.Body{unknownRecord(string(event.Kind), event.Payload)}
	}
	var body transcript.Body
	var ok bool
	switch event.Kind {
	case protocol.KindHarnessStarted:
		body, ok = decodeBody(event.Payload, func(p protocol.HarnessStarted) transcript.Body { return transcript.HarnessStarted(p) })
	case protocol.KindHarnessExited:
		body, ok = decodeBody(event.Payload, func(p protocol.HarnessExited) transcript.Body { return transcript.HarnessExited(p) })
	case protocol.KindPermissionRequested:
		body, ok = decodeBody(event.Payload, func(p protocol.PermissionRequested) transcript.Body { return transcript.PermissionRequested(p) })
	case protocol.KindPauseAcknowledged:
		body, ok = decodeBody(event.Payload, func(p protocol.PauseAcknowledged) transcript.Body { return transcript.PauseAcknowledged(p) })
	case protocol.KindPauseSettled:
		body, ok = decodeBody(event.Payload, func(p protocol.PauseSettled) transcript.Body { return transcript.PauseSettled(p) })
	case protocol.KindQuotaObserved:
		body, ok = decodeBody(event.Payload, func(p protocol.QuotaObserved) transcript.Body { return transcript.QuotaObserved(p) })
	}
	if !ok {
		return []transcript.Body{unknownRecord(string(event.Kind), event.Payload)}
	}
	return []transcript.Body{body}
}

func commandBody(command protocol.Command) transcript.Body {
	var body transcript.Body
	ok := true
	switch command.Kind {
	case protocol.CommandStartTask:
		body, ok = decodeBody(command.Payload, func(p protocol.StartTask) transcript.Body { return transcript.OwnerPrompt{Text: p.Prompt} })
	case protocol.CommandPrompt:
		body, ok = decodeBody(command.Payload, func(p protocol.Prompt) transcript.Body { return transcript.OwnerPrompt{Text: p.Text} })
	case protocol.CommandResume:
		body = transcript.OwnerPrompt{Resume: true}
	case protocol.CommandPause:
		body = transcript.PauseRequested{}
	case protocol.CommandInterrupt:
		body = transcript.Interrupted{}
	case protocol.CommandStop:
		body = transcript.StopRequested{}
	case protocol.CommandAnswerPermission:
		body, ok = decodeBody(command.Payload, func(p protocol.AnswerPermission) transcript.Body { return transcript.PermissionAnswered(p) })
	default:
		ok = false
	}
	if !ok {
		return unknownRecord(string(command.Kind), command.Payload)
	}
	return body
}

// decodeBody decodes payload as P and converts it; it reports false when
// the payload is not a P.
func decodeBody[P any](payload json.RawMessage, convert func(P) transcript.Body) (transcript.Body, bool) {
	var p P
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, false
	}
	return convert(p), true
}

func unknownRecord(kind string, payload json.RawMessage) transcript.Unknown {
	return transcript.Unknown{RecordKind: kind, Raw: payload}
}

// watchers signals the readers of a task's transcript that it may have
// changed.
type watchers struct {
	mu     sync.Mutex
	byTask map[protocol.TaskID]map[chan struct{}]struct{}
}

// WatchTask returns a channel that receives a signal whenever an event of
// task is stored or a command to it issued. Signals coalesce: one signal
// may stand for several changes, so the reader should read the whole
// transcript again. stop ends the watch.
func (s *Server) WatchTask(task protocol.TaskID) (changed <-chan struct{}, stop func()) {
	signal := make(chan struct{}, 1)
	s.watchers.mu.Lock()
	defer s.watchers.mu.Unlock()
	if s.watchers.byTask[task] == nil {
		s.watchers.byTask[task] = make(map[chan struct{}]struct{})
	}
	s.watchers.byTask[task][signal] = struct{}{}
	return signal, func() {
		s.watchers.mu.Lock()
		defer s.watchers.mu.Unlock()
		delete(s.watchers.byTask[task], signal)
		if len(s.watchers.byTask[task]) == 0 {
			delete(s.watchers.byTask, task)
		}
	}
}

// taskChanged signals every watcher of task. It is called after the change
// is committed.
func (s *Server) taskChanged(task protocol.TaskID) {
	s.watchers.mu.Lock()
	defer s.watchers.mu.Unlock()
	for signal := range s.watchers.byTask[task] {
		select {
		case signal <- struct{}{}:
		default:
		}
	}
}
