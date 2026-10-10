package server

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// SchedulePolicy is what the scheduler admits turns by
// (docs/adr/2026-10-08-scheduling.md).
type SchedulePolicy struct {
	// SlotsPerDaemon is the slot count of a daemon that has none of its
	// own.
	SlotsPerDaemon int
	// FillerThreshold and LowThreshold are the five-hour window's
	// utilization, from 0 to 1, that filler and low-priority turns need
	// to stay below.
	FillerThreshold float64
	LowThreshold    float64
	// DaemonTimeout is how long a daemon may go unseen, with no command
	// stream open, before it is lost and its tasks are moved
	// (docs/adr/2026-10-08-daemon-loss.md). Zero declares no daemon lost.
	DaemonTimeout time.Duration
}

// DefaultSchedulePolicy is the policy the scheduling and daemon loss
// records name.
var DefaultSchedulePolicy = SchedulePolicy{SlotsPerDaemon: 2, FillerThreshold: 0.5, LowThreshold: 0.85, DaemonTimeout: DefaultDaemonTimeout}

// fiveHourWindow names the window the thresholds apply to.
const fiveHourWindow = "five_hour"

// quotaReading is the account's newest quota_observed, taken at At.
type quotaReading struct {
	protocol.QuotaObserved
	At time.Time
}

// budget is what a reading allows at a moment.
type budget struct {
	// rejectedUntil is set while a rejected reading holds every turn.
	rejectedUntil time.Time
	// fiveHour is the five-hour window's utilization; known says whether
	// a reading for it applies.
	fiveHour float64
	known    bool
	// changes is the next time a window of the reading resets, when the
	// budget changes without a new reading; zero when none will.
	changes time.Time
}

// readBudget applies the budget rules to reading at now. A rejected
// reading holds every turn until its earliest window resets, and no
// longer applies after; one without windows says nothing about when it
// lifts, so it holds nothing. A window whose reset has passed has no
// figures.
func readBudget(reading *quotaReading, now time.Time) budget {
	var b budget
	if reading == nil {
		return b
	}
	var earliest time.Time
	for _, window := range reading.Windows {
		if earliest.IsZero() || window.ResetsAt.Before(earliest) {
			earliest = window.ResetsAt
		}
		if window.ResetsAt.After(now) && (b.changes.IsZero() || window.ResetsAt.Before(b.changes)) {
			b.changes = window.ResetsAt
		}
	}
	if reading.Status == protocol.QuotaRejected {
		if earliest.After(now) {
			b.rejectedUntil = earliest
		}
		return b
	}
	for _, window := range reading.Windows {
		if window.Name == fiveHourWindow && window.ResetsAt.After(now) {
			b.fiveHour, b.known = window.Utilization, true
		}
	}
	return b
}

// pendingTurn is a turn waiting to be admitted, with what the scheduler
// needs of its task.
type pendingTurn struct {
	ID      uint64
	Task    protocol.TaskID
	Kind    turnKind
	Payload json.RawMessage
	Filler  bool
	Reason  string
	// State, Daemon, Placement and PausedBy are the task's.
	State     TaskState
	Priority  Priority
	Daemon    protocol.DaemonID
	Placement placement
	PausedBy  pauseOrigin
	// Ran lists the daemons a start was issued to before, for a task that
	// was moved off a lost daemon. Its start goes to none of them: one
	// that came back may still hold the task's old record.
	Ran []protocol.DaemonID
	// Requires are the labels the task's daemon must have
	// (docs/adr/2026-10-09-agents-and-placement.md).
	Requires Labels
	// Models are the models its start chooses from, most preferred
	// first; nil when its model was given
	// (docs/adr/2026-10-10-agent-models-and-capacity.md).
	Models []string
}

// slotHolder is a task holding a slot on its daemon.
type slotHolder struct {
	Task     protocol.TaskID
	Daemon   protocol.DaemonID
	State    TaskState
	Filler   bool
	PausedBy pauseOrigin
	// Admitted is the ID of the command that admitted its newest turn;
	// a greater one was admitted later.
	Admitted uint64
}

// holdsSlot reports whether a task in state holds a slot on its daemon:
// from the admission of its turn until its process exits.
func holdsSlot(state TaskState) bool {
	return state == TaskPending || state == TaskRunning || state == TaskAwaitingPermission || state == TaskPausing
}

// schedule is everything the scheduler reads to decide.
type schedule struct {
	// slots maps each connected daemon to its slot count, labels to its
	// facts and the owner's labels, merged, and models to the models it
	// advertises.
	slots   map[protocol.DaemonID]int
	labels  map[protocol.DaemonID]Labels
	models  map[protocol.DaemonID][]string
	holders []slotHolder
	// turns are the waiting turns of tasks that have not ended, oldest
	// first.
	turns   []pendingTurn
	reading *quotaReading
}

// admission is a turn to admit on a daemon. model, set for a start
// that chooses its model, is the one chosen.
type admission struct {
	turn   pendingTurn
	daemon protocol.DaemonID
	model  string
}

// decisions are what the scheduler does in one pass: the turns it
// admits, the filler tasks it yields, why each other turn waits, and when
// the budget next changes on its own.
type decisions struct {
	admit   []admission
	yield   []slotHolder
	reasons map[uint64]string
	wake    time.Time
}

// slotWait is a turn that would be admitted but for a free slot. It
// waits on daemon, unless flexible, when any connected daemon's slot
// would do, daemon's first if it is set, other than those in exclude.
type slotWait struct {
	daemon   protocol.DaemonID
	flexible bool
	exclude  []protocol.DaemonID
}

// decide applies the scheduling rules to s at now:
//
//  1. A rejected budget holds every turn.
//  2. Each task offers its oldest waiting turn other than a delivery, or
//     else its delivery once it is finished; its other turns wait.
//  3. Non-filler turns, highest priority and oldest first, then filler
//     turns likewise, each pass the budget rule for its kind.
//  4. A turn for a task holding a slot rides on it. Any other turn takes
//     a free slot on its placement's daemon.
//  5. Filler turns wait while a non-filler turn waits only for a slot.
//  6. Each such non-filler turn yields the newest running filler task on
//     its daemon, unless a yield there is under way.
func decide(s schedule, policy SchedulePolicy, now time.Time) decisions {
	b := readBudget(s.reading, now)
	d := decisions{reasons: make(map[uint64]string), wake: b.changes}
	if !b.rejectedUntil.IsZero() {
		reason := "rejected by the account's usage limits until " + b.rejectedUntil.UTC().Format("2006-01-02 15:04 MST")
		for _, turn := range s.turns {
			d.reasons[turn.ID] = reason
		}
		d.wake = b.rejectedUntil
		return d
	}

	free := make(map[protocol.DaemonID]int, len(s.slots))
	for daemon, slots := range s.slots {
		free[daemon] = slots
	}
	for _, holder := range s.holders {
		if _, ok := free[holder.Daemon]; ok {
			free[holder.Daemon]--
		}
	}

	var candidates []pendingTurn
	offered := make(map[protocol.TaskID]bool)
	for _, turn := range s.turns {
		if turn.Kind != turnDeliver && !offered[turn.Task] {
			offered[turn.Task] = true
			candidates = append(candidates, turn)
		}
	}
	isCandidate := make(map[uint64]bool, len(candidates))
	for _, turn := range candidates {
		isCandidate[turn.ID] = true
	}
	for _, turn := range s.turns {
		switch {
		case turn.Kind != turnDeliver:
			if !isCandidate[turn.ID] {
				d.reasons[turn.ID] = "waits behind the task's earlier turn"
			}
		case offered[turn.Task] || turn.State != TaskFinished:
			d.reasons[turn.ID] = deliveryWait(turn)
		default:
			offered[turn.Task] = true
			candidates = append(candidates, turn)
		}
	}
	slices.SortFunc(candidates, turnOrder)

	var waits []slotWait
	for _, turn := range candidates {
		if turn.Filler && len(waits) > 0 {
			d.reasons[turn.ID] = "priority: non-filler turns are waiting for a slot"
			continue
		}
		if reason := budgetWait(turn, b, policy); reason != "" {
			d.reasons[turn.ID] = reason
			continue
		}
		if turn.Kind != turnStart && holdsSlot(turn.State) {
			if turn.State == TaskPausing && turn.PausedBy == pauseByScheduler {
				d.reasons[turn.ID] = "waits for the scheduler's pause of the task to settle"
				continue
			}
			d.admit = append(d.admit, admission{turn: turn, daemon: turn.Daemon})
			continue
		}
		daemon, model, reason, wait := place(turn, s, free)
		if daemon != "" {
			free[daemon]--
			d.admit = append(d.admit, admission{turn: turn, daemon: daemon, model: model})
			continue
		}
		d.reasons[turn.ID] = reason
		if wait != nil && !turn.Filler {
			waits = append(waits, *wait)
		}
	}
	d.yield = yields(waits, s)
	return d
}

// deliveryWait says why a delivery waits for its task.
func deliveryWait(turn pendingTurn) string {
	switch turn.State {
	case TaskPaused:
		return "waits for the owner to resume the task"
	case TaskYielded:
		return "waits for the scheduler to resume the task"
	case TaskFinished:
		return "waits behind the task's earlier turn"
	}
	return "waits for the task's turn to end"
}

// budgetWait says why the budget holds turn, or returns "" when it does
// not.
func budgetWait(turn pendingTurn, b budget, policy SchedulePolicy) string {
	switch {
	case turn.Filler && !b.known:
		return "budget: filler needs a current reading of the five-hour window"
	case turn.Filler && b.fiveHour >= policy.FillerThreshold:
		return fmt.Sprintf("budget: the five-hour window is %s used; filler runs below %s", percent(b.fiveHour), percent(policy.FillerThreshold))
	case turn.Priority == PriorityLow && b.known && b.fiveHour >= policy.LowThreshold:
		return fmt.Sprintf("budget: the five-hour window is %s used; low priority runs below %s", percent(b.fiveHour), percent(policy.LowThreshold))
	}
	return ""
}

func percent(fraction float64) string {
	return fmt.Sprintf("%.0f%%", fraction*100)
}

// place picks the daemon with a free slot that turn goes to, and, for a
// start that chooses its model, the model. When there is none it
// returns why, and, if the turn waits only for a slot, what it waits on.
// A start goes only to a daemon whose labels hold what the task
// requires, and to none the task ran on before. One that chooses its
// model takes the first of its models that such a daemon provides, and
// goes only to a daemon that provides it
// (docs/adr/2026-10-10-agent-models-and-capacity.md).
func place(turn pendingTurn, s schedule, free map[protocol.DaemonID]int) (protocol.DaemonID, string, string, *slotWait) {
	_, connected := s.slots[turn.Daemon]
	choosing := turn.Kind == turnStart && len(turn.Models) > 0
	if turn.Kind != turnStart || turn.Placement == placementBound {
		if !connected {
			return "", "", "daemon " + string(turn.Daemon) + " is not connected", nil
		}
		if lacks := lacking(turn.Requires, s.labels[turn.Daemon]); turn.Kind == turnStart && len(lacks) > 0 {
			return "", "", "daemon " + string(turn.Daemon) + " does not have " + lacks.String(), nil
		}
		var model string
		if choosing {
			if model = s.choose(turn.Models, []protocol.DaemonID{turn.Daemon}); model == "" {
				return "", "", "daemon " + string(turn.Daemon) + " does not have model " + alternatives(turn.Models), nil
			}
		}
		if free[turn.Daemon] > 0 {
			return turn.Daemon, model, "", nil
		}
		return "", "", "slots: daemon " + string(turn.Daemon) + " has no free slot", &slotWait{daemon: turn.Daemon}
	}
	exclude := slices.Clone(turn.Ran)
	var candidates, eligible []protocol.DaemonID
	for daemon := range s.slots {
		switch {
		case slices.Contains(turn.Ran, daemon):
		case !s.labels[daemon].Holds(turn.Requires):
			exclude = append(exclude, daemon)
			candidates = append(candidates, daemon)
		default:
			candidates = append(candidates, daemon)
			eligible = append(eligible, daemon)
		}
	}
	var model string
	if choosing && len(eligible) > 0 {
		if model = s.choose(turn.Models, eligible); model == "" {
			return "", "", "waiting for a daemon with model " + alternatives(turn.Models), nil
		}
		for _, daemon := range eligible {
			if !s.serves(daemon, model) {
				exclude = append(exclude, daemon)
			}
		}
	}
	parent := connected && turn.Placement == placementParent && !slices.Contains(exclude, turn.Daemon)
	if parent && free[turn.Daemon] > 0 {
		return turn.Daemon, model, "", nil
	}
	var best protocol.DaemonID
	usable := 0
	for daemon := range s.slots {
		if slices.Contains(exclude, daemon) {
			continue
		}
		usable++
		if free[daemon] > 0 && (best == "" || free[daemon] > free[best] || free[daemon] == free[best] && daemon < best) {
			best = daemon
		}
	}
	if best != "" {
		return best, model, "", nil
	}
	if len(s.slots) == 0 {
		return "", "", "no daemon is connected", nil
	}
	if len(candidates) == 0 {
		return "", "", "the task ran on every connected daemon before it was moved; it waits for another daemon", nil
	}
	if usable == 0 {
		return "", "", "no daemon has " + unmet(turn.Requires, candidates, s.labels).String(), nil
	}
	wait := &slotWait{flexible: true, exclude: exclude}
	if parent {
		wait.daemon = turn.Daemon
	}
	return "", "", "slots: no connected daemon has a free slot", wait
}

// serves reports whether daemon provides model, an entry of an agent's
// models.
func (s schedule) serves(daemon protocol.DaemonID, model string) bool {
	return serves(s.labels[daemon][protocol.FactHarness], s.models[daemon], model)
}

// choose returns the first of models that one of daemons provides, or ""
// when none does.
func (s schedule) choose(models []string, daemons []protocol.DaemonID) string {
	for _, model := range models {
		if slices.ContainsFunc(daemons, func(daemon protocol.DaemonID) bool { return s.serves(daemon, model) }) {
			return model
		}
	}
	return ""
}

// lacking returns the pairs of required that labels does not have.
func lacking(required, labels Labels) Labels {
	lacks := Labels{}
	for key, value := range required {
		if labels[key] != value {
			lacks[key] = value
		}
	}
	return lacks
}

// unmet returns the pairs of required that none of daemons has, or, when
// each is on one of them but none has them all, required.
func unmet(required Labels, daemons []protocol.DaemonID, labels map[protocol.DaemonID]Labels) Labels {
	missing := Labels{}
	for key, value := range required {
		if !slices.ContainsFunc(daemons, func(daemon protocol.DaemonID) bool { return labels[daemon][key] == value }) {
			missing[key] = value
		}
	}
	if len(missing) == 0 {
		return required
	}
	return missing
}

// yields picks the filler tasks to pause so that each wait gets a slot.
// A yield already under way on a daemon serves one wait there. A
// flexible wait takes its own daemon if that has a yield under way or a
// running filler task, else a daemon with a yield under way, else the
// one whose newest running filler task was admitted last, of the daemons
// its task did not run on before.
func yields(waits []slotWait, s schedule) []slotHolder {
	underway := make(map[protocol.DaemonID]int)
	fillers := make(map[protocol.DaemonID][]slotHolder)
	for _, holder := range s.holders {
		if _, ok := s.slots[holder.Daemon]; !ok {
			continue
		}
		switch {
		case holder.State == TaskPausing && holder.PausedBy == pauseByScheduler:
			underway[holder.Daemon]++
		case holder.Filler && (holder.State == TaskRunning || holder.State == TaskAwaitingPermission):
			fillers[holder.Daemon] = append(fillers[holder.Daemon], holder)
		}
	}
	for daemon := range fillers {
		slices.SortFunc(fillers[daemon], func(a, b slotHolder) int { return cmp.Compare(b.Admitted, a.Admitted) })
	}
	usable := func(daemon protocol.DaemonID) bool {
		return daemon != "" && (underway[daemon] > 0 || len(fillers[daemon]) > 0)
	}
	var chosen []slotHolder
	for _, wait := range waits {
		daemon := wait.daemon
		if wait.flexible && !usable(daemon) {
			daemon = ""
			for candidate, count := range underway {
				if count > 0 && !slices.Contains(wait.exclude, candidate) && (daemon == "" || candidate < daemon) {
					daemon = candidate
				}
			}
			var newest uint64
			for candidate, list := range fillers {
				if underway[daemon] == 0 && len(list) > 0 && !slices.Contains(wait.exclude, candidate) && (daemon == "" || list[0].Admitted > newest) {
					daemon, newest = candidate, list[0].Admitted
				}
			}
		}
		switch {
		case !usable(daemon):
		case underway[daemon] > 0:
			underway[daemon]--
		default:
			chosen = append(chosen, fillers[daemon][0])
			fillers[daemon] = fillers[daemon][1:]
		}
	}
	return chosen
}

// passResult is what one pass did beyond admitting turns: the daemons it
// declared lost, the tasks it moved off lost daemons, and when the next
// pass is due on its own, as the budget changes or a daemon would be
// lost; zero when none is.
type passResult struct {
	wake  time.Time
	lost  []protocol.DaemonID
	moved []protocol.TaskID
}

// schedule admits what the scheduling rules allow at now, with connected
// the daemons whose command streams are open, in one transaction. First
// it declares lost the daemons away for longer than policy allows, since
// upSince, and moves the tasks on lost daemons that have work to do
// (docs/adr/2026-10-08-daemon-loss.md).
func (s *Store) schedule(ctx context.Context, policy SchedulePolicy, now time.Time, connected []protocol.DaemonID, upSince time.Time) (passResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return passResult{}, fmt.Errorf("schedule: %w", err)
	}
	defer tx.Rollback()
	var fx effects
	l, err := findLost(ctx, tx, connected, now, upSince, policy.DaemonTimeout)
	if err != nil {
		return passResult{}, err
	}
	moved, err := moveLostTasks(ctx, tx, l.lost, now, &fx)
	if err != nil {
		return passResult{}, err
	}
	state, err := readSchedule(ctx, tx, policy, connected)
	if err != nil {
		return passResult{}, err
	}
	d := decide(state, policy, now)
	for _, admitted := range d.admit {
		if err := admit(ctx, tx, admitted, &fx); err != nil {
			return passResult{}, err
		}
	}
	for _, holder := range d.yield {
		if _, err := yieldTask(ctx, tx, holder.Daemon, holder.Task, &fx); err != nil {
			return passResult{}, err
		}
	}
	for _, turn := range state.turns {
		reason, waits := d.reasons[turn.ID]
		if !waits || reason == turn.Reason {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE turns SET reason = ? WHERE id = ?`, reason, int64(turn.ID)); err != nil {
			return passResult{}, fmt.Errorf("record why turn %d waits: %w", turn.ID, err)
		}
		fx.changed = append(fx.changed, turn.Task)
	}
	if err := tx.Commit(); err != nil {
		return passResult{}, fmt.Errorf("schedule: %w", err)
	}
	// The scheduler's own commands need no further pass unless they
	// changed what it can admit, which issuing them did.
	s.publish(&fx)
	wake := d.wake
	if !l.next.IsZero() && (wake.IsZero() || l.next.Before(wake)) {
		wake = l.next
	}
	return passResult{wake: wake, lost: l.newly, moved: moved}, nil
}

// readSchedule reads what the scheduler decides on.
func readSchedule(ctx context.Context, tx *sql.Tx, policy SchedulePolicy, connected []protocol.DaemonID) (schedule, error) {
	s := schedule{
		slots:  make(map[protocol.DaemonID]int, len(connected)),
		labels: make(map[protocol.DaemonID]Labels, len(connected)),
		models: make(map[protocol.DaemonID][]string, len(connected)),
	}
	for _, daemon := range connected {
		var slots sql.NullInt64
		var labels, facts string
		err := tx.QueryRowContext(ctx, `SELECT slots, labels, facts FROM daemons WHERE id = ?`, string(daemon)).Scan(&slots, &labels, &facts)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return schedule{}, fmt.Errorf("read slots of daemon %q: %w", daemon, err)
		}
		s.slots[daemon] = policy.SlotsPerDaemon
		if slots.Valid {
			s.slots[daemon] = int(slots.Int64)
		}
		owners, err := decodeLabels(labels)
		if err != nil {
			return schedule{}, fmt.Errorf("read labels of daemon %q: %w", daemon, err)
		}
		reported, err := decodeLabels(facts)
		if err != nil {
			return schedule{}, fmt.Errorf("read facts of daemon %q: %w", daemon, err)
		}
		s.labels[daemon] = Merge(reported, owners)
		s.models[daemon] = advertisedModels(reported, owners)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT t.id, t.daemon_id, t.state, t.filler, coalesce(t.pause_origin, ''),
			coalesce((SELECT max(u.admitted_command_id) FROM turns u WHERE u.task_id = t.id), 0)
		FROM tasks t WHERE t.state IN (?, ?, ?, ?)`,
		string(TaskPending), string(TaskRunning), string(TaskAwaitingPermission), string(TaskPausing))
	if err != nil {
		return schedule{}, fmt.Errorf("read tasks holding slots: %w", err)
	}
	for rows.Next() {
		var holder slotHolder
		var task, daemon, state, pausedBy string
		var admitted int64
		if err := rows.Scan(&task, &daemon, &state, &holder.Filler, &pausedBy, &admitted); err != nil {
			rows.Close()
			return schedule{}, fmt.Errorf("read tasks holding slots: %w", err)
		}
		holder.Task, holder.Daemon, holder.State, holder.PausedBy, holder.Admitted =
			protocol.TaskID(task), protocol.DaemonID(daemon), TaskState(state), pauseOrigin(pausedBy), uint64(admitted)
		s.holders = append(s.holders, holder)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return schedule{}, fmt.Errorf("read tasks holding slots: %w", err)
	}

	if s.turns, err = queryWaitingTurns(ctx, tx); err != nil {
		return schedule{}, err
	}
	if s.reading, err = queryReading(ctx, tx); err != nil {
		return schedule{}, err
	}
	return s, nil
}

// queryWaitingTurns returns the waiting turns of tasks the owner has not
// dismissed, oldest first. A task placed by its parent's daemon has that
// daemon as its own, as it is now: the parent may have been moved since
// the task was spawned. A daemon id holds no comma, so the daemons a
// task's starts went to are read as one comma-separated list. A start the
// owner queued for a task that ran before is a retry of a task whose
// start failed on the daemon it is bound to, which ended the task for
// good there, so that daemon is not among those the task ran on.
func queryWaitingTurns(ctx context.Context, tx *sql.Tx) ([]pendingTurn, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT u.id, u.task_id, u.kind, u.payload, u.filler, u.reason,
			t.state, t.priority,
			CASE WHEN t.placement = ?1 THEN coalesce((SELECT p.daemon_id FROM tasks p WHERE p.id = t.parent_id), t.daemon_id) ELSE t.daemon_id END,
			t.placement, coalesce(t.pause_origin, ''),
			coalesce((SELECT group_concat(DISTINCT c.daemon_id) FROM commands c WHERE c.task_id = t.id AND c.kind = ?2
				AND NOT (u.kind = ?2 AND u.origin = ?3 AND c.daemon_id = t.daemon_id)), ''),
			t.requires, t.models
		FROM turns u JOIN tasks t ON t.id = u.task_id
		WHERE u.admitted_command_id IS NULL AND t.dismissed_at IS NULL
		ORDER BY u.id`, string(placementParent), string(protocol.CommandStartTask), string(originOwner))
	if err != nil {
		return nil, fmt.Errorf("read waiting turns: %w", err)
	}
	defer rows.Close()
	var turns []pendingTurn
	for rows.Next() {
		var turn pendingTurn
		var id int64
		var task, kind, state, priority, daemon, placed, pausedBy, ran, requires string
		var models sql.NullString
		var payload []byte
		if err := rows.Scan(&id, &task, &kind, &payload, &turn.Filler, &turn.Reason, &state, &priority, &daemon, &placed, &pausedBy, &ran, &requires, &models); err != nil {
			return nil, fmt.Errorf("read waiting turns: %w", err)
		}
		var err error
		if turn.Requires, err = decodeLabels(requires); err != nil {
			return nil, fmt.Errorf("read waiting turn %d: %w", id, err)
		}
		if models.Valid {
			if err := json.Unmarshal([]byte(models.String), &turn.Models); err != nil {
				return nil, fmt.Errorf("read waiting turn %d: models: %w", id, err)
			}
		}
		turn.ID, turn.Task, turn.Kind, turn.Payload = uint64(id), protocol.TaskID(task), turnKind(kind), payload
		turn.State, turn.Priority, turn.Daemon, turn.Placement, turn.PausedBy =
			TaskState(state), Priority(priority), protocol.DaemonID(daemon), placement(placed), pauseOrigin(pausedBy)
		if ran != "" {
			for daemon := range strings.SplitSeq(ran, ",") {
				turn.Ran = append(turn.Ran, protocol.DaemonID(daemon))
			}
		}
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read waiting turns: %w", err)
	}
	return turns, nil
}

// queryReading returns the newest quota reading any daemon reported, or
// nil when there is none or it does not decode. Stored times keep the
// daemon's zone offset and trim trailing zeros, so they do not sort as
// text; julianday compares the instants, to the millisecond, and the row
// id, which follows storage order, breaks ties.
func queryReading(ctx context.Context, tx *sql.Tx) (*quotaReading, error) {
	var at, payload string
	err := tx.QueryRowContext(ctx, `
		SELECT time, payload FROM events WHERE kind = ? ORDER BY julianday(time) DESC, rowid DESC LIMIT 1`,
		string(protocol.KindQuotaObserved)).Scan(&at, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the quota reading: %w", err)
	}
	var reading quotaReading
	taken, err := parseTime(at)
	if err != nil || json.Unmarshal([]byte(payload), &reading.QuotaObserved) != nil {
		return nil, nil
	}
	reading.At = taken
	return &reading, nil
}

// admit issues the command of a.turn on a.daemon and records the turn
// admitted. A start binds its task to a.daemon, and records a.model, when
// set, as the task's model and the start's.
func admit(ctx context.Context, tx *sql.Tx, a admission, fx *effects) error {
	turn := a.turn
	var command protocol.Command
	var err error
	switch turn.Kind {
	case turnStart:
		payload := turn.Payload
		if a.model != "" {
			if payload, err = withModel(payload, a.model); err != nil {
				return fmt.Errorf("start task %q: %w", turn.Task, err)
			}
			if _, err = tx.ExecContext(ctx, `UPDATE tasks SET model = ? WHERE id = ?`, a.model, string(turn.Task)); err != nil {
				return fmt.Errorf("record the model of task %q: %w", turn.Task, err)
			}
		}
		// A task moved off a lost daemon has events from there; the new
		// daemon numbers its own from 1, so they are stored after those.
		_, err = tx.ExecContext(ctx, `
			UPDATE tasks SET daemon_id = ?1, placement = ?2,
				seq_base = coalesce((SELECT max(seq) FROM events WHERE task_id = ?3), 0)
			WHERE id = ?3`,
			string(a.daemon), string(placementBound), string(turn.Task))
		if err != nil {
			return fmt.Errorf("place task %q: %w", turn.Task, err)
		}
		command, err = insertCommand(ctx, tx, a.daemon, turn.Task, protocol.CommandStartTask, payload, fx)
	case turnPrompt:
		command, err = insertCommand(ctx, tx, a.daemon, turn.Task, protocol.CommandPrompt, turn.Payload, fx)
	case turnResume:
		command, err = insertCommand(ctx, tx, a.daemon, turn.Task, protocol.CommandResume, nil, fx)
	case turnDeliver:
		command, err = deliverWaiting(ctx, tx, a.daemon, turn.Task, fx)
	default:
		err = fmt.Errorf("turn %d has unknown kind %q", turn.ID, turn.Kind)
	}
	if err != nil {
		return err
	}
	if command.ID == 0 {
		// A delivery whose messages have all gone out has nothing to do.
		_, err = tx.ExecContext(ctx, `DELETE FROM turns WHERE id = ?`, int64(turn.ID))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE turns SET admitted_command_id = ?, reason = '' WHERE id = ?`, int64(command.ID), int64(turn.ID))
	}
	if err != nil {
		return fmt.Errorf("record turn %d admitted: %w", turn.ID, err)
	}
	return nil
}

// withModel is the start_task payload with model as its model.
func withModel(payload json.RawMessage, model string) (json.RawMessage, error) {
	var start protocol.StartTask
	if err := json.Unmarshal(payload, &start); err != nil {
		return nil, fmt.Errorf("read the start: %w", err)
	}
	start.Model = model
	encoded, err := json.Marshal(start)
	if err != nil {
		return nil, fmt.Errorf("encode the start: %w", err)
	}
	return encoded, nil
}

// scheduler runs passes of Store.schedule whenever it is woken, and when
// the budget changes on its own.
type scheduler struct {
	store     *Store
	log       *slog.Logger
	policy    SchedulePolicy
	now       func() time.Time
	connected func() []protocol.DaemonID
	// upSince is when the server started, by now: a daemon's absence
	// counts towards its loss from then at the earliest.
	upSince time.Time
	// wake holds a signal when something may have changed what the
	// scheduler can admit.
	wake chan struct{}
}

// poke asks for a pass.
func (s *scheduler) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// pass runs one pass and returns when the next is due on its own.
func (s *scheduler) pass(ctx context.Context) (time.Time, error) {
	result, err := s.store.schedule(ctx, s.policy, s.now(), s.connected(), s.upSince)
	if err != nil {
		return time.Time{}, err
	}
	for _, daemon := range result.lost {
		s.log.Warn("daemon lost: not seen and not connected", "daemon", daemon, "timeout", s.policy.DaemonTimeout)
	}
	for _, task := range result.moved {
		s.log.Info("task moved off a lost daemon; its next turn starts it afresh on another", "task", task)
	}
	return result.wake, nil
}

// retryAfter is how long the scheduler waits after a failed pass.
const retryAfter = time.Second

// run runs passes until ctx ends.
func (s *scheduler) run(ctx context.Context) {
	timer := time.NewTimer(0)
	timer.Stop()
	defer timer.Stop()
	for {
		next, err := s.pass(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Error("schedule turns", "error", err)
			next = s.now().Add(retryAfter)
		}
		var due <-chan time.Time
		if next.IsZero() {
			timer.Stop()
		} else {
			// The budget changes at next by the scheduler's clock; the
			// timer runs on the real one.
			timer.Reset(max(next.Sub(s.now()), 0))
			due = timer.C
		}
		select {
		case <-s.wake:
		case <-due:
		case <-ctx.Done():
			return
		}
	}
}
