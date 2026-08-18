package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// Watch kinds. A watch names what to look at, not what to do about it.
const (
	// WatchKindBranch matches branch refs against a glob pattern.
	WatchKindBranch = "branch"
	// WatchKindPR watches open pull requests. Its pattern is empty.
	WatchKindPR = "pr"
)

// Watch is one registered unit of QA work for a repository: what to look at,
// and in which mode to validate what it finds.
//
// Watches are stored rather than derived so a watch has a stable ID that
// observed state can hang off, and so the CLI can enable, disable, and remove
// one without rewriting the operator's configuration file.
type Watch struct {
	ID      string
	RepoID  string
	Kind    string
	Pattern string
	Mode    string
	Enabled bool
	// PollIntervalSeconds overrides the global poll interval for this watch.
	// Nil means "use the global default", which is different from zero.
	PollIntervalSeconds *int64
	// DailyAt is an "HH:MM" local wall-clock anchor, or empty for none.
	DailyAt   string
	CreatedAt int64
	UpdatedAt int64
}

// WatchState is what a watch has already observed for one ref. It is the
// crash-safety record for unattended work.
//
// LastSeenSHA advances only after a run for that SHA reaches a terminal status,
// so a crash mid-run leaves it at the previous value and the next sweep
// re-derives the same work. That is what makes it safe to simply fail a crashed
// QA run rather than trying to resume it.
type WatchState struct {
	WatchID             string
	Ref                 string
	LastSeenSHA         string
	LastRunID           string
	LastStatus          string
	LastRunAt           *int64
	ConsecutiveFailures int64
	UpdatedAt           int64
}

const watchColumns = `id, repo_id, kind, pattern, mode, enabled, poll_interval_seconds, COALESCE(daily_at, ''), created_at, updated_at`

func scanWatch(s rowScanner) (*Watch, error) {
	w := &Watch{}
	var enabled int64
	if err := s.Scan(
		&w.ID, &w.RepoID, &w.Kind, &w.Pattern, &w.Mode, &enabled,
		&w.PollIntervalSeconds, &w.DailyAt, &w.CreatedAt, &w.UpdatedAt,
	); err != nil {
		return nil, err
	}
	w.Enabled = enabled != 0
	return w, nil
}

const watchStateColumns = `watch_id, ref, last_seen_sha, COALESCE(last_run_id, ''), COALESCE(last_status, ''), last_run_at, consecutive_failures, updated_at`

func scanWatchState(s rowScanner) (*WatchState, error) {
	st := &WatchState{}
	if err := s.Scan(
		&st.WatchID, &st.Ref, &st.LastSeenSHA, &st.LastRunID, &st.LastStatus,
		&st.LastRunAt, &st.ConsecutiveFailures, &st.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return st, nil
}

// InsertWatch registers a watch. The table's UNIQUE (repo_id, kind, pattern,
// mode) constraint makes registering the same watch twice an error rather than a
// silent duplicate that would validate every ref twice per sweep.
func (d *DB) InsertWatch(repoID, kind, pattern, mode string, pollIntervalSeconds *int64, dailyAt string) (*Watch, error) {
	w := &Watch{
		ID:                  newID(),
		RepoID:              repoID,
		Kind:                strings.TrimSpace(kind),
		Pattern:             strings.TrimSpace(pattern),
		Mode:                strings.TrimSpace(mode),
		Enabled:             true,
		PollIntervalSeconds: pollIntervalSeconds,
		DailyAt:             strings.TrimSpace(dailyAt),
		CreatedAt:           now(),
	}
	w.UpdatedAt = w.CreatedAt
	var interval any
	if w.PollIntervalSeconds != nil {
		interval = *w.PollIntervalSeconds
	}
	_, err := d.sql.Exec(
		`INSERT INTO repo_watches (id, repo_id, kind, pattern, mode, enabled, poll_interval_seconds, daily_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
		w.ID, w.RepoID, w.Kind, w.Pattern, w.Mode, interval, nullableString(w.DailyAt), w.CreatedAt, w.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert watch: %w", err)
	}
	return w, nil
}

// GetWatches returns every registered watch ordered by repository then ID.
func (d *DB) GetWatches() ([]*Watch, error) {
	return d.queryWatches(`SELECT ` + watchColumns + ` FROM repo_watches ORDER BY repo_id, id`)
}

// GetWatchesByRepo returns every watch registered for one repository.
func (d *DB) GetWatchesByRepo(repoID string) ([]*Watch, error) {
	return d.queryWatches(`SELECT `+watchColumns+` FROM repo_watches WHERE repo_id = ? ORDER BY id`, repoID)
}

func (d *DB) queryWatches(query string, args ...any) ([]*Watch, error) {
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get watches: %w", err)
	}
	defer rows.Close()

	var watches []*Watch
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, fmt.Errorf("scan watch: %w", err)
		}
		watches = append(watches, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate watches: %w", err)
	}
	return watches, nil
}

// GetWatch returns a watch by ID, or nil when none exists.
func (d *DB) GetWatch(id string) (*Watch, error) {
	w, err := scanWatch(d.sql.QueryRow(`SELECT `+watchColumns+` FROM repo_watches WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get watch: %w", err)
	}
	return w, nil
}

// DeleteWatch removes a watch and, by cascade, its observed state.
func (d *DB) DeleteWatch(id string) error {
	if _, err := d.sql.Exec(`DELETE FROM repo_watches WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete watch: %w", err)
	}
	return nil
}

// SetWatchEnabled enables or disables a watch without discarding its observed
// state, so re-enabling one does not re-validate every ref it already covered.
func (d *DB) SetWatchEnabled(id string, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	if _, err := d.sql.Exec(`UPDATE repo_watches SET enabled = ?, updated_at = ? WHERE id = ?`, value, now(), id); err != nil {
		return fmt.Errorf("set watch enabled: %w", err)
	}
	return nil
}

// GetWatchState returns every ref a watch has observed, keyed by ref.
func (d *DB) GetWatchState(watchID string) (map[string]*WatchState, error) {
	rows, err := d.sql.Query(`SELECT `+watchStateColumns+` FROM watch_state WHERE watch_id = ?`, watchID)
	if err != nil {
		return nil, fmt.Errorf("get watch state: %w", err)
	}
	defer rows.Close()

	states := make(map[string]*WatchState)
	for rows.Next() {
		st, err := scanWatchState(rows)
		if err != nil {
			return nil, fmt.Errorf("scan watch state: %w", err)
		}
		states[st.Ref] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate watch state: %w", err)
	}
	return states, nil
}

// AdvanceWatchState records that a ref was validated at a SHA and clears the
// failure streak.
//
// Callers must only advance after the run reached a terminal status AND the ref
// still points at the SHA that was dispatched. Advancing earlier would let a
// crash, a cancellation, or a mid-run force-push silently retire work that was
// never actually validated.
func (d *DB) AdvanceWatchState(watchID, ref, sha, runID, status string) error {
	ts := now()
	_, err := d.sql.Exec(
		`INSERT INTO watch_state (watch_id, ref, last_seen_sha, last_run_id, last_status, last_run_at, consecutive_failures, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 0, ?)
		 ON CONFLICT(watch_id, ref) DO UPDATE SET
		     last_seen_sha = excluded.last_seen_sha,
		     last_run_id = excluded.last_run_id,
		     last_status = excluded.last_status,
		     last_run_at = excluded.last_run_at,
		     consecutive_failures = 0,
		     updated_at = excluded.updated_at`,
		watchID, ref, sha, nullableString(runID), nullableString(status), ts, ts,
	)
	if err != nil {
		return fmt.Errorf("advance watch state: %w", err)
	}
	return nil
}

// RecordWatchFailure increments a ref's failure streak WITHOUT advancing
// last_seen_sha, so the work stays outstanding and is re-derived next sweep. The
// streak is what a caller backs off on, so a repository whose agent is broken
// does not burn an agent invocation every poll.
func (d *DB) RecordWatchFailure(watchID, ref, runID, status string) error {
	ts := now()
	_, err := d.sql.Exec(
		`INSERT INTO watch_state (watch_id, ref, last_seen_sha, last_run_id, last_status, last_run_at, consecutive_failures, updated_at)
		 VALUES (?, ?, '', ?, ?, ?, 1, ?)
		 ON CONFLICT(watch_id, ref) DO UPDATE SET
		     last_run_id = excluded.last_run_id,
		     last_status = excluded.last_status,
		     last_run_at = excluded.last_run_at,
		     consecutive_failures = watch_state.consecutive_failures + 1,
		     updated_at = excluded.updated_at`,
		watchID, ref, nullableString(runID), nullableString(status), ts, ts,
	)
	if err != nil {
		return fmt.Errorf("record watch failure: %w", err)
	}
	return nil
}
