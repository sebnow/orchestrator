package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

const (
	// defaultBatchEvents and defaultBatchBytes bound one POST of events,
	// well under the server's 32 MiB body limit. An event larger than the
	// byte bound is sent alone.
	defaultBatchEvents = 500
	defaultBatchBytes  = 4 << 20
	// requestTimeout bounds one request to the server other than the
	// command stream.
	requestTimeout = 30 * time.Second
)

// errRejected reports a request the server refused as malformed or not
// this daemon's. Sending it again cannot succeed.
var errRejected = errors.New("rejected by the server")

// movedError reports a batch the server refused because it moved the
// batch's task to another daemon after declaring this one lost
// (docs/adr/2026-10-08-daemon-loss.md). The task is no longer this
// daemon's.
type movedError struct {
	task    protocol.TaskID
	message string
}

func (e *movedError) Error() string {
	return fmt.Sprintf("the server moved task %s to another daemon: %s", e.task, e.message)
}

// sender sends every task's journal to the server, in seq order per task,
// and records how far the server holds each. A task whose journal ends in
// harness_exited and is held whole has its journal deleted, and is
// forgotten unless it can be resumed (docs/adr/2026-10-08-task-lifetime.md).
//
// After any failure to reach the server it waits with backoff, and its
// next attempt starts with GET /acks so that every task is replayed from
// what the server holds.
type sender struct {
	client   *http.Client
	server   *url.URL
	daemon   protocol.DaemonID
	stateDir string
	state    *state
	log      *slog.Logger
	backoff  backoff

	batchEvents int
	batchBytes  int

	// moved, when set, hears of each task the server refused as moved to
	// another daemon. The sender stops sending the task.
	moved func(protocol.TaskID)

	wake chan struct{}
	mu   sync.Mutex
	// told holds the tasks notify has named since the last pass.
	told map[protocol.TaskID]bool

	// Owned by run.
	outboxes  map[protocol.TaskID]*outbox
	connected bool
}

// outbox is the sender's position in one task's journal.
type outbox struct {
	path string
	// offset is where the first event after acked starts, or an earlier
	// line boundary.
	offset int64
	acked  uint64
	// exited is set while the last event the server holds is
	// harness_exited: the journal ends there unless a new process of the
	// task appends to it.
	exited bool
	// rejected stops sending a task the server refused.
	rejected bool
}

// journalLine is one event as journaled, with the offset just after it.
type journalLine struct {
	raw  []byte
	seq  uint64
	kind protocol.Kind
	end  int64
}

func newSender(client *http.Client, server *url.URL, daemon protocol.DaemonID, stateDir string, st *state, log *slog.Logger, b backoff) *sender {
	s := &sender{
		client:      client,
		server:      server,
		daemon:      daemon,
		stateDir:    stateDir,
		state:       st,
		log:         log,
		backoff:     b,
		batchEvents: defaultBatchEvents,
		batchBytes:  defaultBatchBytes,
		wake:        make(chan struct{}, 1),
		told:        map[protocol.TaskID]bool{},
		outboxes:    map[protocol.TaskID]*outbox{},
	}
	for _, task := range st.tasks() {
		s.told[task] = true
	}
	return s
}

// notify tells the sender that task's journal has grown. It never blocks.
func (s *sender) notify(task protocol.TaskID) {
	s.mu.Lock()
	s.told[task] = true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run sends until ctx ends. Once drain is closed it returns as soon as
// everything journaled has been sent and acknowledged.
func (s *sender) run(ctx context.Context, drain <-chan struct{}) {
	for {
		err := s.pass(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.connected = false
			s.log.Warn("send events", "error", err)
			if s.backoff.wait(ctx) != nil {
				return
			}
			continue
		}
		s.backoff.reset()
		select {
		case <-drain:
			return
		default:
		}
		select {
		case <-s.wake:
		case <-drain:
		case <-ctx.Done():
			return
		}
	}
}

// pass sends every event journaled so far. It returns an error only when
// the server could not be reached or answered with a server error.
func (s *sender) pass(ctx context.Context) error {
	s.mu.Lock()
	for task := range s.told {
		// A task the state does not know has been forgotten, or was
		// never accepted.
		if _, ok := s.outboxes[task]; !ok && s.state.known(task) {
			s.outboxes[task] = &outbox{path: JournalPath(s.stateDir, task), acked: s.state.acked(task)}
		}
	}
	clear(s.told)
	s.mu.Unlock()

	if !s.connected {
		held, err := s.getAcks(ctx)
		if err != nil {
			return err
		}
		for task, ob := range s.outboxes {
			s.replayFrom(task, ob, held[task])
		}
		s.connected = true
	}

	for _, task := range slices.Sorted(maps.Keys(s.outboxes)) {
		if err := s.sendTask(ctx, task, s.outboxes[task]); err != nil {
			return err
		}
	}
	return nil
}

// replayFrom moves ob to what the server holds. When the server holds
// less than the daemon recorded, the journal is read again from the start.
func (s *sender) replayFrom(task protocol.TaskID, ob *outbox, held uint64) {
	if held < ob.acked {
		s.log.Warn("server holds fewer events than it acknowledged; sending them again",
			"task", task, "held", held, "acknowledged", ob.acked)
		ob.offset, ob.exited = 0, false
	}
	s.setAcked(task, ob, held)
}

func (s *sender) setAcked(task protocol.TaskID, ob *outbox, held uint64) {
	if held == ob.acked {
		return
	}
	ob.acked = held
	if err := s.state.recordAcked(task, held); err != nil {
		// The watermark only saves resending; the server ignores
		// duplicates.
		s.log.Error("record acknowledged events", "task", task, "error", err)
	}
}

// sendTask sends task's unsent events in batches, then deletes its
// journal if its process has ended and the server holds all of it.
func (s *sender) sendTask(ctx context.Context, task protocol.TaskID, ob *outbox) error {
	for !ob.rejected {
		lines, err := s.readBatch(ob)
		if err != nil {
			s.log.Error("read journal; its events will not be sent", "task", task, "error", err)
			ob.rejected = true
			return nil
		}
		if len(lines) == 0 {
			break
		}
		held, err := s.postEvents(ctx, lines)
		if _, ok := errors.AsType[*movedError](err); ok {
			s.log.Warn("server moved the task to another daemon; dropping it here", "task", task, "error", err)
			ob.rejected = true
			if s.moved != nil {
				s.moved(task)
			}
			return nil
		}
		if errors.Is(err, errRejected) {
			s.log.Error("server refused events; they will not be sent again", "task", task,
				"first_seq", lines[0].seq, "last_seq", lines[len(lines)-1].seq, "error", err)
			ob.rejected = true
			return nil
		}
		if err != nil {
			return err
		}
		h := held[task]
		if h < lines[0].seq {
			return fmt.Errorf("server holds task %s up to seq %d after receiving seq %d to %d", task, h, lines[0].seq, lines[len(lines)-1].seq)
		}
		for _, line := range lines {
			if line.seq <= h {
				ob.offset = line.end
				ob.exited = line.kind == protocol.KindHarnessExited
			}
		}
		s.setAcked(task, ob, max(h, ob.acked))
	}
	if ob.exited && !ob.rejected {
		s.dropJournal(task, ob)
	}
	return nil
}

// dropJournal deletes the journal of a task whose process has ended and
// which the server holds whole. The state keeps the task when it can be
// resumed; a later process starts a new journal after the held seq. The
// state refuses while a new process holds the journal.
func (s *sender) dropJournal(task protocol.TaskID, ob *outbox) {
	dropped, err := s.state.dropJournal(task, ob.acked, ob.path)
	if err != nil {
		s.log.Error("delete journal", "task", task, "error", err)
		return
	}
	if dropped {
		delete(s.outboxes, task)
	}
}

// readBatch reads the next events after ob.acked from the journal, up to
// the batch bounds. It stops before a line still being written. Events up
// to ob.acked that it passes over move ob.offset on.
func (s *sender) readBatch(ob *outbox) ([]journalLine, error) {
	file, err := os.Open(ob.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Seek(ob.offset, io.SeekStart); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(file)
	var lines []journalLine
	offset, size := ob.offset, 0
	for len(lines) < s.batchEvents {
		raw, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		var head struct {
			Seq  uint64        `json:"seq"`
			Kind protocol.Kind `json:"kind"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, fmt.Errorf("journal %s at byte %d: %w", ob.path, offset, err)
		}
		offset += int64(len(raw))
		if head.Seq <= ob.acked {
			ob.offset = offset
			ob.exited = head.Kind == protocol.KindHarnessExited
			continue
		}
		if len(lines) > 0 && size+len(raw) > s.batchBytes {
			break
		}
		lines = append(lines, journalLine{raw: raw, seq: head.Seq, kind: head.Kind, end: offset})
		size += len(raw)
	}
	return lines, nil
}

func (s *sender) postEvents(ctx context.Context, lines []journalLine) (map[protocol.TaskID]uint64, error) {
	var body bytes.Buffer
	body.WriteByte('[')
	for idx, line := range lines {
		if idx > 0 {
			body.WriteByte(',')
		}
		body.Write(line.raw)
	}
	body.WriteByte(']')
	return s.request(ctx, http.MethodPost, "events", &body)
}

func (s *sender) getAcks(ctx context.Context) (map[protocol.TaskID]uint64, error) {
	return s.request(ctx, http.MethodGet, "acks", nil)
}

// request calls one of the daemon's endpoints, which all answer with the
// held seq per task.
func (s *sender) request(ctx context.Context, method, endpoint string, body io.Reader) (map[protocol.TaskID]uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	target := s.server.JoinPath("v1", "daemons", string(s.daemon), endpoint)
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	if resp.StatusCode == http.StatusConflict {
		var refused protocol.EventsRefused
		if json.Unmarshal(data, &refused) == nil && refused.Reason == protocol.RefusedTaskMoved {
			return nil, &movedError{task: refused.TaskID, message: refused.Message}
		}
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return nil, fmt.Errorf("%w: %s %s: %s: %s", errRejected, method, endpoint, resp.Status, bytes.TrimSpace(data))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: %s: %s", method, endpoint, resp.Status, bytes.TrimSpace(data))
	}
	var held map[protocol.TaskID]uint64
	if err := json.Unmarshal(data, &held); err != nil {
		return nil, fmt.Errorf("%s %s: decode response: %w", method, endpoint, err)
	}
	return held, nil
}

// backoff spaces out attempts to reach the server: each wait is up to
// twice the last, from min up to max, with jitter so that daemons cut
// off together do not return together.
type backoff struct {
	min, max time.Duration
	next     time.Duration
}

func (b *backoff) wait(ctx context.Context) error {
	if b.next < b.min {
		b.next = b.min
	}
	delay := b.next/2 + rand.N(b.next/2+1)
	b.next = min(b.next*2, b.max)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *backoff) reset() {
	b.next = b.min
}
