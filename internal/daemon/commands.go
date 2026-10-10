package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

const (
	// streamIdleTimeout ends a command stream that has sent nothing, not
	// even a keepalive comment, for this long, so that a connection lost
	// without a reset is noticed. The server sends a keepalive every 15 s.
	streamIdleTimeout = 45 * time.Second
	// maxStreamLine bounds one line of the command stream. A start_task
	// carries at most the server's 1 MiB owner request, escaped.
	maxStreamLine = 16 << 20
)

// resumePrompt resumes a paused task.
const resumePrompt = "Resume the task from where you stopped and finish it."

// cutShortNote tells an agent resumed after the daemon cut its turn short,
// by dying or by shutting down, why the turn ended.
const cutShortNote = " Your last turn was cut short when the daemon running you stopped; check what it left unfinished."

// interruptedNote tells an agent resumed after the owner's interrupt cut
// its turn short why the turn ended.
const interruptedNote = " The owner interrupted your last turn; check what it left unfinished."

// resumeText is the prompt that resumes a task with record rec: the stop
// note of its settled pause, or why its turn was cut short.
func resumeText(rec taskRecord) string {
	text := resumePrompt
	switch {
	case rec.CutShort && rec.Interrupted:
		text += interruptedNote
	case rec.CutShort:
		text += cutShortNote
	}
	if rec.Paused && rec.StopNote != "" {
		text += " Your note when you stopped: " + rec.StopNote
	}
	return text
}

// receive applies the commands the server sends until ctx ends,
// reconnecting with backoff whenever the stream ends.
func (s *service) receive(ctx context.Context) {
	b := backoff{min: s.cfg.MinBackoff, max: s.cfg.MaxBackoff}
	for {
		err := s.streamCommands(ctx, &b)
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("command stream ended", "error", err)
		if b.wait(ctx) != nil {
			return
		}
	}
}

// streamCommands reports the daemon's facts, then opens the command
// stream, sending the id of the last command applied as Last-Event-ID,
// and applies each command the stream carries. It resets b once the
// server has accepted the stream. Facts the server does not take are
// logged and do not hold up the stream: placement only lacks them.
func (s *service) streamCommands(ctx context.Context, b *backoff) error {
	if err := s.reportFacts(ctx); err != nil {
		s.log.Warn("report facts", "error", err)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	idle := time.AfterFunc(streamIdleTimeout, func() { cancel(errors.New("command stream idle")) })
	defer idle.Stop()

	target := s.cfg.Server.JoinPath("v1", "daemons", string(s.cfg.ID), "commands")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if last := s.lastApplied(); last > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(last, 10))
	}
	resp, err := s.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("open command stream: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	b.reset()
	err = readEvents(resp.Body, func() { idle.Reset(streamIdleTimeout) }, func(id, data string) error {
		return s.receiveCommand(id, data)
	})
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if err == nil {
		return errors.New("server closed the command stream")
	}
	return err
}

// receiveCommand applies one command from the stream unless its id has
// been applied already. A command that cannot be decoded is logged and
// skipped. An error ends the stream, so that the command is sent again.
func (s *service) receiveCommand(rawID, data string) error {
	id, err := strconv.ParseUint(rawID, 10, 64)
	if err != nil {
		s.log.Error("command without a valid id; skipped", "id", rawID)
		return nil
	}
	if id <= s.lastApplied() {
		return nil
	}
	var command protocol.Command
	if err := json.Unmarshal([]byte(data), &command); err != nil {
		s.log.Error("command not decodable; skipped", "command", id, "error", err)
		s.recordApplied(id)
		return nil
	}
	command.ID = id
	if command.Kind == protocol.CommandStartTask {
		return s.accept(command)
	}
	s.route(command)
	return nil
}

// readEvents reads a server-sent event stream, calling seen for every
// line, comments included, and dispatch for every event with data, with
// its data lines joined by newlines and the last event id the stream set.
// Event types and retry fields are ignored. It returns nil at the end of
// the stream, and dispatch's error if it returns one.
func readEvents(r io.Reader, seen func(), dispatch func(id, data string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, maxStreamLine)
	var id string
	var data strings.Builder
	hasData := false
	for scanner.Scan() {
		seen()
		line := scanner.Text()
		if line == "" {
			if hasData {
				if err := dispatch(id, data.String()); err != nil {
					return err
				}
			}
			data.Reset()
			hasData = false
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			if !strings.ContainsRune(value, 0) {
				id = value
			}
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			hasData = true
		}
	}
	return scanner.Err()
}

// apply applies a command to task, whose process is t, or nil when it
// has none, and returns the task's process afterwards. A command that
// cannot apply is logged and dropped.
func (s *service) apply(task protocol.TaskID, t *Task, command protocol.Command) *Task {
	if t != nil && isDone(t) {
		s.processEnded(task, t, false)
		t = nil
	}
	t, err := s.applyCommand(task, t, command)
	if err != nil {
		s.log.Warn("command not applied", "command", command.ID, "kind", command.Kind, "task", task, "error", err)
		return t
	}
	s.log.Info("command applied", "command", command.ID, "kind", command.Kind, "task", task)
	return t
}

// errNoProcess reports a command that needs a running process while the
// task is between processes.
var errNoProcess = errors.New("the task has no running process")

func (s *service) applyCommand(task protocol.TaskID, t *Task, command protocol.Command) (*Task, error) {
	switch command.Kind {
	case protocol.CommandStartTask:
		if t != nil {
			return t, errors.New("the task is already running")
		}
		if rec, _ := s.state.record(task); rec.Settings != nil || rec.Seq > 0 {
			return nil, errors.New("the task has already started")
		}
		return s.startTask(command), nil
	case protocol.CommandPrompt:
		prompt, err := decodePayload[protocol.Prompt](command)
		if err != nil {
			return t, err
		}
		if prompt.Steer {
			return s.steer(task, t, prompt.Text)
		}
		return s.prompt(task, t, command.ID, prompt.Text)
	case protocol.CommandWithdraw:
		withdraw, err := decodePayload[protocol.Withdraw](command)
		if err != nil {
			return t, err
		}
		if t == nil {
			return nil, fmt.Errorf("%w: %d", ErrNotHeld, withdraw.Prompt)
		}
		return t, t.Withdraw(withdraw.Prompt)
	case protocol.CommandResume:
		if t != nil {
			if err := s.waitForPauseToSettle(t); err != nil {
				return t, err
			}
			if state := t.State(); !state.Closing && state.Running {
				return t, errors.New("task is not paused")
			}
			s.awaitExit(task, t)
		}
		// The owner sees the task as paused, or pausing, when it issues a
		// resume; a task whose pause did not take effect before its turn
		// ended resumes all the same, so that it does not stay running
		// with no process.
		return s.resume(task, "")
	case protocol.CommandPause:
		if t == nil {
			return nil, errNoProcess
		}
		return t, t.Pause()
	case protocol.CommandInterrupt:
		if t == nil {
			return nil, errNoProcess
		}
		return t, t.Interrupt(s.cutShortByInterrupt(task))
	case protocol.CommandStop:
		if t == nil {
			return nil, s.stopIdle(task)
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		t.Stop(ctx, nil)
		s.processEnded(task, t, true)
		return nil, nil
	case commandDropMoved, protocol.CommandDiscard:
		if t != nil {
			t.Kill()
			s.processEnded(task, t, true)
		}
		s.forget(task)
		return nil, nil
	case protocol.CommandAnswerPermission:
		answer, err := decodePayload[protocol.AnswerPermission](command)
		if err != nil {
			return t, err
		}
		if t == nil {
			return nil, fmt.Errorf("%w: %s", ErrStale, answer.RequestID)
		}
		return t, t.AnswerPermission(answer.RequestID, harness.Decision{Allow: answer.Allow, Message: answer.Message})
	default:
		return t, fmt.Errorf("unknown command kind %q", command.Kind)
	}
}

// prompt holds text, which command ref carried, until t's running turn
// ends, when t sends it as the next turn's prompt. Once t's turn is over,
// or when the task has no process, it waits for t to exit and resumes the
// task in a new process with text.
func (s *service) prompt(task protocol.TaskID, t *Task, ref uint64, text string) (*Task, error) {
	if t != nil {
		if err := s.waitForPauseToSettle(t); err != nil {
			return t, err
		}
		err := t.Hold(ref, text)
		if !errors.Is(err, ErrTaskEnded) {
			return t, err
		}
		s.awaitExit(task, t)
	}
	return s.resume(task, text)
}

// steer interrupts t's running turn and sends text as the next prompt in
// the same session. Once t's turn is over, or when the task has no
// process, there is nothing to interrupt, and it resumes the task with
// text as prompt does.
func (s *service) steer(task protocol.TaskID, t *Task, text string) (*Task, error) {
	if t != nil {
		if err := s.waitForPauseToSettle(t); err != nil {
			return t, err
		}
		err := t.Steer(text)
		if !errors.Is(err, ErrTaskEnded) {
			return t, err
		}
		s.awaitExit(task, t)
	}
	return s.resume(task, text)
}

// stopIdle stops a task that has no process. It journals a
// harness_exited, so that the server records the stop even when it still
// shows the task running. The task keeps its session and workspace, so
// that the owner can follow it up until dismissing it.
func (s *service) stopIdle(task protocol.TaskID) error {
	j, err := s.openTaskJournal(task)
	if err != nil {
		return err
	}
	_, err = j.appendControl(protocol.KindHarnessExited, protocol.HarnessExited{ExitCode: -1, Error: "stopped with no process running"})
	j.close()
	s.journalClosed(task, j)
	return err
}

func isDone(t *Task) bool {
	select {
	case <-t.Done():
		return true
	default:
		return false
	}
}

// waitForPauseToSettle waits while a pause is in progress, so that a
// prompt or resume the owner sent during it applies once it has taken
// effect rather than being refused.
func (s *service) waitForPauseToSettle(t *Task) error {
	_, err := t.WaitFor(s.stopping, func(state State) bool {
		inProgress := state.Pause == PauseRequested || state.Pause == PauseAcknowledged || state.Pause == PauseInterrupting
		return !state.Running || !inProgress
	})
	return err
}

func decodePayload[T any](command protocol.Command) (T, error) {
	var payload T
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		return payload, fmt.Errorf("decode %s payload: %w", command.Kind, err)
	}
	return payload, nil
}
