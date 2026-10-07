package server

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/component"
	"github.com/sebnow/orchestrator/internal/html"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// cursor is how far into a task's transcript a page has got: the highest
// event seq and command id among the entries it shows. On the wire it is
// "<seq>-<command id>".
//
// An event stored late with a seq below the cursor's, after a gap was
// filled, is not sent; the page shows it once reloaded.
type cursor struct {
	seq, command uint64
}

func (c cursor) String() string {
	return strconv.FormatUint(c.seq, 10) + "-" + strconv.FormatUint(c.command, 10)
}

func parseCursor(raw string) (cursor, error) {
	if raw == "" {
		return cursor{}, nil
	}
	rawSeq, rawCommand, found := strings.Cut(raw, "-")
	seq, seqErr := strconv.ParseUint(rawSeq, 10, 63)
	command, commandErr := strconv.ParseUint(rawCommand, 10, 63)
	if !found || seqErr != nil || commandErr != nil {
		return cursor{}, fmt.Errorf("%q is not a transcript position", raw)
	}
	return cursor{seq: seq, command: command}, nil
}

// after returns the entries past c, in transcript order, and the cursor
// past them.
func (c cursor) after(entries []transcript.Entry) ([]transcript.Entry, cursor) {
	var fresh []transcript.Entry
	next := c
	for _, entry := range entries {
		source := entry.Source
		switch {
		case source.CommandID != 0 && source.CommandID > c.command:
			next.command = max(next.command, source.CommandID)
		case source.CommandID == 0 && source.Seq > c.seq:
			next.seq = max(next.seq, source.Seq)
		default:
			continue
		}
		fresh = append(fresh, entry)
	}
	return fresh, next
}

// changesPermission reports whether entry can change which permission
// requests wait for an answer.
func changesPermission(entry transcript.Entry) bool {
	switch entry.Body.(type) {
	case transcript.PermissionRequested, transcript.PermissionAnswered, transcript.HarnessExited:
		return true
	}
	return false
}

// update renders what a task page at c lacks: the entries after c, to
// append to its transcript, and the regions that may have changed. The
// permission prompt is replaced only when it may have changed, so that a
// reason being typed into it survives other updates. polling says the
// page has fallen back from server-sent events to polling. It returns the
// cursor past the entries.
func (v taskView) update(c cursor, polling bool) (html.Node, cursor) {
	fresh, next := c.after(v.entries)
	var permission html.Node
	if slices.ContainsFunc(fresh, changesPermission) {
		permission = component.OutOfBand(component.RegionPermission, v.permission(""))
	}
	live := component.OutOfBand(component.RegionFallback, component.SSEFallback(v.id(), next.String()))
	if polling {
		live = component.OutOfBand(component.RegionLive, component.Polling(v.id(), next.String()))
	}
	return html.Fragment(
		component.TranscriptEntries(fresh),
		component.OutOfBand(component.RegionTaskHeader, v.header()),
		permission,
		component.OutOfBand(component.RegionPromptSubmit, component.PromptSubmit(v.detail.State == TaskPausing)),
		live,
	), next
}

// streamTask sends a task page its updates as server-sent events, each
// with the cursor past it as its id: first what the page lacks, if
// anything, then an update whenever the task changes. The page's cursor is
// the Last-Event-ID a reconnecting browser sends, or else the "after"
// query parameter.
func (s *Server) streamTask(w http.ResponseWriter, r *http.Request) {
	task, err := protocol.ParseTaskID(r.PathValue("task"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	position := r.Header.Get("Last-Event-ID")
	if position == "" {
		position = r.URL.Query().Get("after")
	}
	at, err := parseCursor(position)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The watch starts before the first read, so a change in between is
	// signalled rather than missed.
	changed, stop := s.WatchTask(task)
	defer stop()
	view, err := s.readTaskView(r.Context(), task)
	if errors.Is(err, errUnknownTask) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	sender := http.NewResponseController(w)
	if err := sender.Flush(); err != nil {
		return
	}
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()
	for first := true; ; first = false {
		if !first {
			if view, err = s.readTaskView(r.Context(), task); err != nil {
				// Ending the stream makes the page fall back to polling.
				if r.Context().Err() == nil {
					s.log.Error("read task for its page", "task", task, "error", err)
				}
				return
			}
		}
		fragment, next := view.update(at, false)
		if !first || next != at {
			if err := writeEvent(w, next.String(), fragment); err != nil {
				return
			}
			if err := sender.Flush(); err != nil {
				return
			}
		}
		at = next

	wait:
		for {
			select {
			case <-changed:
				break wait
			case <-keepalive.C:
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
				if err := sender.Flush(); err != nil {
					return
				}
			case <-s.ended:
				return
			case <-r.Context().Done():
				return
			}
		}
	}
}

// writeEvent writes node as one server-sent event with id. Each line of
// the HTML goes on a data line of its own; carriage returns become line
// feeds, as an HTML parser would read them anyway, because a bare one
// would end a data line early.
func writeEvent(w http.ResponseWriter, id string, node html.Node) error {
	var buf bytes.Buffer
	if err := html.Render(&buf, node); err != nil {
		return err
	}
	data := strings.ReplaceAll(strings.ReplaceAll(buf.String(), "\r\n", "\n"), "\r", "\n")
	var event strings.Builder
	event.WriteString("id: " + id + "\n")
	for line := range strings.SplitSeq(data, "\n") {
		event.WriteString("data: " + line + "\n")
	}
	event.WriteString("\n")
	_, err := w.Write([]byte(event.String()))
	return err
}

// getTaskUpdates answers a page that polls instead of streaming with the
// update after its cursor, the "after" query parameter.
func (s *Server) getTaskUpdates(w http.ResponseWriter, r *http.Request) {
	at, err := parseCursor(r.URL.Query().Get("after"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	view, ok := s.taskViewFromPath(w, r)
	if !ok {
		return
	}
	fragment, _ := view.update(at, true)
	s.writeHTML(w, http.StatusOK, fragment)
}
