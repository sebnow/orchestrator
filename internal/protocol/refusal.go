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
)

// EventsRefused is the JSON body of the server's 409 Conflict answer to a
// POSTed batch of events. The server stores none of the batch. TaskID is
// the task the batch was refused for, and Message says why in words.
type EventsRefused struct {
	Reason  RefusalReason `json:"reason"`
	TaskID  TaskID        `json:"task_id"`
	Message string        `json:"message"`
}
