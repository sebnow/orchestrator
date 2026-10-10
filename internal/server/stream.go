package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// streamRefusedError says why the server refused to open a daemon's
// command stream (docs/adr/2026-10-10-server-loss.md).
type streamRefusedError struct {
	Reason  protocol.RefusalReason
	Message string
}

func (e *streamRefusedError) Error() string {
	return e.Message
}

// streamRefusal is a refusal as the daemon list keeps it.
type streamRefusal struct {
	Reason  protocol.RefusalReason `json:"reason"`
	Message string                 `json:"message"`
	At      time.Time              `json:"at"`
}

// openCommandStream records daemon as seen and decides whether its
// command stream opens, given the position of the last command it
// applied: the zero position for none, and one without an epoch for a
// command applied before commands had epochs, which is in the first
// epoch. It returns that position when the stream opens, and a
// *streamRefusedError when it does not, which the daemon list shows
// until a stream of the daemon's opens.
//
//   - In the current epoch, the daemon cannot have applied a command the
//     server has not issued, so a position past the last issued is
//     refused as daemon_ahead.
//   - In an earlier epoch of the lineage, the server was restored, and the
//     stream goes on from the position through every later epoch.
//   - An epoch outside the lineage is refused as unknown_lineage: the
//     daemon's state is not from this database's history, as after a
//     restore to a backup older than an earlier restore.
func (s *Store) openCommandStream(ctx context.Context, daemon protocol.DaemonID, last protocol.CommandPosition) (protocol.CommandPosition, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.CommandPosition{}, fmt.Errorf("open command stream: %w", err)
	}
	defer tx.Rollback()
	now := s.now()
	if err := seeDaemon(ctx, tx, daemon, now, nil); err != nil {
		return protocol.CommandPosition{}, err
	}
	refusal, err := judgeStream(ctx, tx, last)
	if err != nil {
		return protocol.CommandPosition{}, err
	}
	var stored any
	if refusal != nil {
		data, err := json.Marshal(streamRefusal{Reason: refusal.Reason, Message: refusal.Message, At: now.UTC()})
		if err != nil {
			return protocol.CommandPosition{}, fmt.Errorf("record the stream refusal: %w", err)
		}
		stored = string(data)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE daemons SET stream_refusal = ? WHERE id = ?`, stored, string(daemon)); err != nil {
		return protocol.CommandPosition{}, fmt.Errorf("record the stream refusal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return protocol.CommandPosition{}, fmt.Errorf("open command stream: %w", err)
	}
	if refusal != nil {
		return protocol.CommandPosition{}, refusal
	}
	return last, nil
}

// judgeStream returns why a stream from last is refused, or nil.
func judgeStream(ctx context.Context, tx *sql.Tx, last protocol.CommandPosition) (*streamRefusedError, error) {
	var current protocol.Epoch
	var currentOrdinal int64
	if err := tx.QueryRowContext(ctx, `SELECT id, ordinal FROM epochs ORDER BY ordinal DESC LIMIT 1`).Scan(&current, &currentOrdinal); err != nil {
		return nil, fmt.Errorf("read the current epoch: %w", err)
	}
	epoch := last.Epoch
	var ordinal int64
	var err error
	if epoch == "" {
		err = tx.QueryRowContext(ctx, `SELECT id, ordinal FROM epochs WHERE ordinal = 1`).Scan(&epoch, &ordinal)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT ordinal FROM epochs WHERE id = ?`, string(epoch)).Scan(&ordinal)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return &streamRefusedError{Reason: protocol.RefusedUnknownLineage, Message: fmt.Sprintf(
			"the daemon last applied command %d of epoch %s, which is not in this server's lineage; the server's current epoch is %s",
			last.ID, last.Epoch, current)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the daemon's epoch: %w", err)
	}
	if ordinal != currentOrdinal {
		return nil, nil
	}
	var issued int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM commands WHERE epoch = ?`, string(epoch)).Scan(&issued); err != nil {
		return nil, fmt.Errorf("read the last command issued: %w", err)
	}
	if last.ID > uint64(issued) {
		return &streamRefusedError{Reason: protocol.RefusedDaemonAhead, Message: fmt.Sprintf(
			"the daemon last applied command %d of epoch %s, the current one, but the server has issued only up to %d in it",
			last.ID, epoch, issued)}, nil
	}
	return nil, nil
}
