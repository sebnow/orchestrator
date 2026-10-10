package protocol

// RefusalReason says why the server refused a daemon's batch of events.
type RefusalReason string

const (
	// RefusedNotAssigned: the batch names a task that is not assigned to
	// the daemon, or that the server does not know.
	RefusedNotAssigned RefusalReason = "not_assigned"
	// RefusedTaskMoved: the batch names a task the server moved from the
	// daemon to another after declaring the daemon lost. The daemon is to
	// stop the task and forget it (docs/adr/2026-10-08-daemon-loss.md).
	RefusedTaskMoved RefusalReason = "task_moved"
	// RefusedDaemonAhead: the daemon's Last-Event-ID names a command of
	// the server's current epoch that the server has not issued. The
	// daemon's state is not from this server.
	RefusedDaemonAhead RefusalReason = "daemon_ahead"
	// RefusedUnknownEpoch: the daemon's Last-Event-ID names an epoch
	// the server's database has no record of, as after a restore to a
	// backup older than an earlier restore. The owner resets the
	// daemon's last command by hand (docs/adr/2026-10-10-server-loss.md).
	RefusedUnknownEpoch RefusalReason = "unknown_epoch"
)

// StreamRefused is the JSON body of the server's 409 Conflict answer to a
// daemon opening its command stream. Message says why in words, and
// names the server's current epoch.
type StreamRefused struct {
	Reason  RefusalReason `json:"reason"`
	Message string        `json:"message"`
}

// EventsRefused is the JSON body of the server's 409 Conflict answer to a
// POSTed batch of events. The server stores none of the batch. TaskID is
// the task the batch was refused for, and Message says why in words.
type EventsRefused struct {
	Reason  RefusalReason `json:"reason"`
	TaskID  TaskID        `json:"task_id"`
	Message string        `json:"message"`
}
