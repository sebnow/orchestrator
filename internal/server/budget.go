package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// budgetKey names a budget (docs/adr/2026-10-10-harness-login.md): the
// harness and the account a daemon reports, or, for a daemon that
// reports no account, the daemon itself. Quota readings are kept per key,
// and the scheduler checks a turn against the budget of the key of the
// daemon it would go to.
type budgetKey struct {
	Harness string
	Account string
	Daemon  protocol.DaemonID
}

// budgetKeyOf is the key of daemon, which reported facts, and whose
// latest event named harness, nil before it sent any: its harness fact,
// or else that harness's name, and its account fact, or else its id.
func budgetKeyOf(daemon protocol.DaemonID, facts Labels, harness *protocol.Harness) budgetKey {
	key := budgetKey{Harness: facts[protocol.FactHarness], Account: facts[protocol.FactAccount]}
	if key.Harness == "" && harness != nil {
		key.Harness = harness.Name
	}
	if key.Account == "" {
		key.Daemon = daemon
	}
	return key
}

// String words the key for the owner: the harness and the account, or
// the daemon.
func (k budgetKey) String() string {
	var parts []string
	if k.Harness != "" {
		parts = append(parts, k.Harness)
	}
	if k.Account != "" {
		parts = append(parts, "account "+k.Account)
	} else {
		parts = append(parts, "daemon "+string(k.Daemon))
	}
	return strings.Join(parts, ", ")
}

// compareKeys orders keys by harness, then account, then daemon.
func compareKeys(a, b budgetKey) int {
	return strings.Compare(a.Harness+"\x00"+a.Account+"\x00"+string(a.Daemon), b.Harness+"\x00"+b.Account+"\x00"+string(b.Daemon))
}

// queryBudgetKey reads the key of daemon, as it is now.
func queryBudgetKey(ctx context.Context, tx *sql.Tx, daemon protocol.DaemonID) (budgetKey, error) {
	var facts string
	var harness sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT facts, harness_name FROM daemons WHERE id = ?`, string(daemon)).Scan(&facts, &harness)
	if err != nil {
		return budgetKey{}, fmt.Errorf("read the budget of daemon %q: %w", daemon, err)
	}
	reported, err := decodeLabels(facts)
	if err != nil {
		return budgetKey{}, fmt.Errorf("read facts of daemon %q: %w", daemon, err)
	}
	var h *protocol.Harness
	if harness.Valid {
		h = &protocol.Harness{Name: harness.String}
	}
	return budgetKeyOf(daemon, reported, h), nil
}

// queryBudgetKeys reads the key of every daemon.
func queryBudgetKeys(ctx context.Context, tx *sql.Tx) (map[protocol.DaemonID]budgetKey, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, facts, harness_name FROM daemons`)
	if err != nil {
		return nil, fmt.Errorf("read budget keys: %w", err)
	}
	defer rows.Close()
	keys := make(map[protocol.DaemonID]budgetKey)
	for rows.Next() {
		var id, facts string
		var harness sql.NullString
		if err := rows.Scan(&id, &facts, &harness); err != nil {
			return nil, fmt.Errorf("read budget keys: %w", err)
		}
		reported, err := decodeLabels(facts)
		if err != nil {
			return nil, fmt.Errorf("read facts of daemon %q: %w", id, err)
		}
		var h *protocol.Harness
		if harness.Valid {
			h = &protocol.Harness{Name: harness.String}
		}
		keys[protocol.DaemonID(id)] = budgetKeyOf(protocol.DaemonID(id), reported, h)
	}
	return keys, rows.Err()
}

// queryReadings returns the newest quota reading of each key, leaving
// out one that does not decode. Stored times keep the daemon's zone
// offset and trim trailing zeros, so they do not sort as text; julianday
// compares the instants, to the millisecond, and the row id, which
// follows storage order, breaks ties.
func queryReadings(ctx context.Context, tx *sql.Tx) (map[budgetKey]*quotaReading, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT budget_harness, coalesce(budget_account, ''), coalesce(budget_daemon, ''), time, payload FROM (
			SELECT budget_harness, budget_account, budget_daemon, time, payload,
				row_number() OVER (PARTITION BY budget_harness, budget_account, budget_daemon ORDER BY julianday(time) DESC, rowid DESC) AS newest
			FROM events WHERE kind = ?)
		WHERE newest = 1`, string(protocol.KindQuotaObserved))
	if err != nil {
		return nil, fmt.Errorf("read quota readings: %w", err)
	}
	defer rows.Close()
	readings := make(map[budgetKey]*quotaReading)
	for rows.Next() {
		var key budgetKey
		var daemon, at, payload string
		if err := rows.Scan(&key.Harness, &key.Account, &daemon, &at, &payload); err != nil {
			return nil, fmt.Errorf("read quota readings: %w", err)
		}
		key.Daemon = protocol.DaemonID(daemon)
		var reading quotaReading
		taken, err := parseTime(at)
		if err != nil || json.Unmarshal([]byte(payload), &reading.QuotaObserved) != nil {
			continue
		}
		reading.At = taken
		readings[key] = &reading
	}
	return readings, rows.Err()
}

// keyedReading is a budget's newest reading.
type keyedReading struct {
	key budgetKey
	*quotaReading
}

// readings returns each budget's newest reading, by key.
func (s *Store) readings(ctx context.Context) ([]keyedReading, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("read quota readings: %w", err)
	}
	defer tx.Rollback()
	byKey, err := queryReadings(ctx, tx)
	if err != nil {
		return nil, err
	}
	var readings []keyedReading
	for key, reading := range byKey {
		readings = append(readings, keyedReading{key: key, quotaReading: reading})
	}
	slices.SortFunc(readings, func(a, b keyedReading) int { return compareKeys(a.key, b.key) })
	return readings, nil
}
