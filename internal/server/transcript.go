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
// stored events and commands, its children's starts, and the messages it
// sent or was sent.
func (s *Server) Transcript(ctx context.Context, task protocol.TaskID) ([]transcript.Entry, error) {
	h, err := s.store.taskHistory(ctx, task)
	if err != nil {
		return nil, err
	}
	return assemble(task, h), nil
}

// assemble merges task's history into one transcript: its events, in seq
// order; its commands and its children's starts, in id order; and the
// messages it sent and the notices that its children ended or that tasks
// ended before its messages reached them, in id order.
// Each list keeps its own order; where they interleave is decided by
// time, in that order of preference when the times are equal. Events are
// stamped by the daemon's clock and the rest by the server's, so the
// interleaving is only as good as their agreement.
//
// A message waiting in task's inbox is left out until the prompt that
// delivers it, where it appears as a MessageReceived.
func assemble(task protocol.TaskID, h history) []transcript.Entry {
	delivered := make(map[uint64][]storedMessage)
	var fromMessages []transcript.Entry
	for _, message := range h.messages {
		source := transcript.Source{TaskID: task, MessageID: message.ID}
		switch {
		case message.From != nil && *message.From == task:
			fromMessages = append(fromMessages, transcript.Entry{Time: message.CreatedAt, Source: source,
				Body: transcript.MessageSent{To: message.To, Text: message.Text}})
		case message.About != nil && message.AboutChild:
			fromMessages = append(fromMessages, transcript.Entry{Time: message.CreatedAt, Source: source,
				Body: transcript.ChildEnded{Child: *message.About, State: string(message.AboutState)}})
		case message.About != nil:
			fromMessages = append(fromMessages, transcript.Entry{Time: message.CreatedAt, Source: source,
				Body: transcript.MessageUndeliverable{To: *message.About, State: string(message.AboutState)}})
		}
		if message.To == task && message.DeliveredBy != 0 {
			delivered[message.DeliveredBy] = append(delivered[message.DeliveredBy], message)
		}
	}
	var fromEvents []transcript.Entry
	for _, event := range h.events {
		source := transcript.Source{TaskID: event.TaskID, Seq: event.Seq}
		for _, body := range eventBodies(event) {
			fromEvents = append(fromEvents, transcript.Entry{Time: event.Time, Source: source, Body: body})
		}
	}
	var fromCommands []transcript.Entry
	for _, command := range h.commands {
		source := transcript.Source{TaskID: command.TaskID, CommandID: command.ID}
		for _, body := range commandBodies(task, h.parent, command, delivered[command.ID]) {
			fromCommands = append(fromCommands, transcript.Entry{Time: command.Time, Source: source, Body: body})
		}
	}
	return mergeByTime(fromEvents, fromCommands, fromMessages)
}

// mergeByTime merges lists that each keep their own order, taking the
// earliest head each time, and the head of the earlier list when times
// are equal.
func mergeByTime(lists ...[]transcript.Entry) []transcript.Entry {
	total := 0
	for _, list := range lists {
		total += len(list)
	}
	entries := make([]transcript.Entry, 0, total)
	for len(entries) < total {
		next := -1
		for idx, list := range lists {
			if len(list) > 0 && (next < 0 || list[0].Time.Before(lists[next][0].Time)) {
				next = idx
			}
		}
		entries = append(entries, lists[next][0])
		lists[next] = lists[next][1:]
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
		body, ok = decodeBody(event.Payload, func(p protocol.HarnessExited) transcript.Body {
			restarted, newSession := restartOf(p)
			return transcript.HarnessExited{ExitCode: p.ExitCode, Error: p.Error, Stderr: p.Stderr, Restarted: restarted, NewSession: newSession}
		})
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

// commandBodies describes a command in task's transcript. A start_task of
// another task is a child of task starting; task's own is spawned by
// parent, when that is set. A prompt with a sender delivered the messages
// in delivered, one entry each; when they cannot be found, the prompt's
// own text and sender stand for them.
func commandBodies(task protocol.TaskID, parent *protocol.TaskID, command protocol.Command, delivered []storedMessage) []transcript.Body {
	var body transcript.Body
	ok := true
	switch command.Kind {
	case protocol.CommandStartTask:
		if command.TaskID != task {
			body, ok = decodeBody(command.Payload, func(p protocol.StartTask) transcript.Body {
				return transcript.ChildSpawned{Child: command.TaskID, Prompt: p.Prompt}
			})
			break
		}
		body, ok = decodeBody(command.Payload, func(p protocol.StartTask) transcript.Body {
			return transcript.OwnerPrompt{Text: p.Prompt, SpawnedBy: parent}
		})
	case protocol.CommandPrompt:
		var prompt protocol.Prompt
		if ok = json.Unmarshal(command.Payload, &prompt) == nil; !ok {
			break
		}
		if prompt.From == nil {
			body = transcript.OwnerPrompt{Text: prompt.Text}
			break
		}
		if len(delivered) == 0 {
			body = transcript.MessageReceived{From: prompt.From, Text: prompt.Text}
			break
		}
		bodies := make([]transcript.Body, len(delivered))
		for idx, message := range delivered {
			bodies[idx] = transcript.MessageReceived{From: message.From, Text: message.Text}
		}
		return bodies
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
		return []transcript.Body{unknownRecord(string(command.Kind), command.Payload)}
	}
	return []transcript.Body{body}
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
