package db

import (
	"fmt"
	"unicode/utf8"
)

const (
	RoundSelectionSourceUser    = "user"
	RoundSelectionSourceAutoFix = "auto_fix"
	// RoundSelectionSourcePolicy is a selection made by an unattended gate
	// policy answering for a run that has no human and no driving agent.
	// Keeping it distinct from "user" is what stops a QA run's own decisions
	// from reading back as a person's.
	RoundSelectionSourcePolicy = "policy"
)

// Gate-action sources. gate_action records how a parked approval gate was
// answered, and gate_action_source records who answered it. Only the policy
// source is written today; the others name the vocabulary the column carries so
// a later writer does not invent a second spelling for the same answer.
const (
	GateActionSourceHuman      = "human"
	GateActionSourcePolicy     = "policy"
	GateActionSourceReconciler = "reconciler"
	GateActionSourceAutoFix    = "auto-fix"
)

// maxGateActionReason bounds the stored reason. A reason is a short explanation
// for a status surface, never a payload: bounding it at the persistence boundary
// keeps every future caller bounded rather than trusting each one.
const maxGateActionReason = 256

// StepRound represents one execution round within a pipeline step.
type StepRound struct {
	ID               string
	StepResultID     string
	Round            int
	Trigger          string  // "initial", "auto_fix"; legacy "user_fix" is treated as "auto_fix"
	FindingsJSON     *string // nullable - findings produced by this round
	ReviewedHeadSHA  *string // non-authoritative commit candidate captured by a review round
	StartingHeadSHA  *string
	TrustedConfigSHA *string
	GlobalConfigYAML []byte
	RepoConfigYAML   []byte
	// UserFindingsJSON, when non-nil, is the merged finding list that was
	// dispatched to the fix agent after the user edited per-finding
	// instructions or added their own findings. It includes both the
	// selected agent-produced findings (with any attached user
	// instructions) and the user-authored findings.
	UserFindingsJSON *string
	// SelectedFindingIDs, when non-nil, is a JSON array of finding IDs that
	// were chosen (by the user or auto-fix filter) to be fixed AFTER this
	// round. It is populated on the round whose findings triggered the next
	// round, so that later rounds' prompts can tell which findings were
	// deliberately left unselected.
	SelectedFindingIDs *string
	SelectionSource    *string
	// FixSummary, when non-nil, is the agent's one-line commit summary for
	// the fix attempt performed during this round. It is only set when the
	// round itself was a fix round (trigger=="auto_fix").
	FixSummary *string
	// GateAction, GateActionSource, and GateActionReason record how this
	// round's approval gate was answered, by whom, and why. All three are nil
	// for a round that never parked at a gate.
	GateAction       *string
	GateActionSource *string
	GateActionReason *string
	DurationMS       int64
	CreatedAt        int64
}

// StepRoundStats summarizes execution rounds for a step. It lets status
// surfaces show whether a running/fixing step is in an initial pass or a fix
// pass without reloading every round in callers.
type StepRoundStats struct {
	TotalRounds        int
	FixRounds          int
	LatestRound        int
	LatestTrigger      string
	LatestSelection    string
	LatestRoundAt      int64
	LatestFixRound     int
	LatestFixRoundAt   int64
	SelectedForFix     bool
	AutoSelectedForFix bool
	PendingFixSource   string
}

// IsFixRound reports whether this round was a fix attempt. Legacy "user_fix"
// rounds count: they were fix rounds dispatched by an explicit user selection.
func (r *StepRound) IsFixRound() bool {
	return r.Trigger == "auto_fix" || r.Trigger == "user_fix"
}

// StepFixSummaries returns one entry per fix round for a step, in round order:
// the agent's one-line fix summary, or "" when the round recorded none.
func (d *DB) StepFixSummaries(stepResultID string) ([]string, error) {
	rounds, err := d.GetRoundsByStep(stepResultID)
	if err != nil {
		return nil, err
	}
	var summaries []string
	for _, r := range rounds {
		if !r.IsFixRound() {
			continue
		}
		summary := ""
		if r.FixSummary != nil {
			summary = *r.FixSummary
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// StepRoundStats returns aggregate round information for a step result.
func (d *DB) StepRoundStats(stepResultID string) (StepRoundStats, error) {
	rounds, err := d.GetRoundsByStep(stepResultID)
	if err != nil {
		return StepRoundStats{}, err
	}
	var stats StepRoundStats
	latestSelectedRound := 0
	latestSelectedSource := ""
	for _, r := range rounds {
		stats.TotalRounds++
		stats.LatestRound = r.Round
		stats.LatestTrigger = r.Trigger
		stats.LatestRoundAt = r.CreatedAt
		if r.SelectionSource != nil {
			stats.LatestSelection = *r.SelectionSource
		}
		if r.SelectedFindingIDs != nil && *r.SelectedFindingIDs != "" {
			stats.SelectedForFix = true
			stats.AutoSelectedForFix = r.SelectionSource != nil && *r.SelectionSource == RoundSelectionSourceAutoFix
			latestSelectedRound = r.Round
			latestSelectedSource = stats.LatestSelection
		}
		if r.IsFixRound() {
			stats.FixRounds++
			stats.LatestFixRound = stats.FixRounds
			stats.LatestFixRoundAt = r.CreatedAt
		}
	}
	if latestSelectedRound == stats.LatestRound {
		stats.PendingFixSource = latestSelectedSource
	}
	return stats, nil
}

// InsertStepRound creates a new round record for a step result. fixSummary may
// be nil for non-fix rounds or when the agent produced no summary.
func (d *DB) InsertStepRound(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary *string, durationMS int64) (*StepRound, error) {
	return d.insertStepRound(stepResultID, round, trigger, findingsJSON, fixSummary, nil, nil, nil, nil, nil, durationMS)
}

// InsertReviewStepRound persists a review round's examined commit as a
// non-authoritative candidate. A recovered parked gate can promote this exact
// candidate only after approval; merely storing it grants no push authority.
func (d *DB) InsertReviewStepRound(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary *string, reviewedHeadSHA string, durationMS int64) (*StepRound, error) {
	return d.InsertReviewStepRoundWithProvenance(stepResultID, round, trigger, findingsJSON, fixSummary, reviewedHeadSHA, "", "", nil, nil, durationMS)
}

func (d *DB) InsertReviewStepRoundWithProvenance(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary *string, reviewedHeadSHA, startingHeadSHA, trustedConfigSHA string, globalConfigYAML, repoConfigYAML []byte, durationMS int64) (*StepRound, error) {
	var reviewed, starting, trusted *string
	if reviewedHeadSHA != "" {
		reviewed = &reviewedHeadSHA
	}
	if startingHeadSHA != "" {
		starting = &startingHeadSHA
	}
	if trustedConfigSHA != "" {
		trusted = &trustedConfigSHA
	}
	return d.insertStepRound(stepResultID, round, trigger, findingsJSON, fixSummary, reviewed, starting, trusted, globalConfigYAML, repoConfigYAML, durationMS)
}

func (d *DB) insertStepRound(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary, reviewedHeadSHA, startingHeadSHA, trustedConfigSHA *string, globalConfigYAML, repoConfigYAML []byte, durationMS int64) (*StepRound, error) {
	r := &StepRound{
		ID:               newID(),
		StepResultID:     stepResultID,
		Round:            round,
		Trigger:          trigger,
		FindingsJSON:     findingsJSON,
		ReviewedHeadSHA:  reviewedHeadSHA,
		StartingHeadSHA:  startingHeadSHA,
		TrustedConfigSHA: trustedConfigSHA,
		GlobalConfigYAML: append([]byte(nil), globalConfigYAML...),
		RepoConfigYAML:   append([]byte(nil), repoConfigYAML...),
		FixSummary:       fixSummary,
		DurationMS:       durationMS,
		CreatedAt:        now(),
	}
	_, err := d.sql.Exec(
		`INSERT INTO step_rounds (id, step_result_id, round, trigger_type, findings_json, reviewed_head_sha, starting_head_sha, trusted_config_sha, global_config_yaml, repo_config_yaml, user_findings_json, selected_finding_ids, selection_source, fix_summary, duration_ms, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.StepResultID, r.Round, r.Trigger, r.FindingsJSON, r.ReviewedHeadSHA, r.StartingHeadSHA, r.TrustedConfigSHA, r.GlobalConfigYAML, r.RepoConfigYAML, r.UserFindingsJSON, r.SelectedFindingIDs, r.SelectionSource, r.FixSummary, r.DurationMS, r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert step round: %w", err)
	}
	return r, nil
}

// SetStepRoundSelection records which findings were selected for fix AFTER the
// given round produced its findings, along with whether that selection came
// from the user or auto-fix filtering. Passing a nil or empty JSON array clears
// both columns.
func (d *DB) SetStepRoundSelection(id string, selectedFindingIDs *string, source string) error {
	var selectionSource *string
	if selectedFindingIDs != nil && *selectedFindingIDs != "" && source != "" {
		selectionSource = &source
	}
	if _, err := d.sql.Exec(
		`UPDATE step_rounds SET selected_finding_ids = ?, selection_source = ? WHERE id = ?`,
		selectedFindingIDs, selectionSource, id,
	); err != nil {
		return fmt.Errorf("set step round selection: %w", err)
	}
	return nil
}

// SetStepRoundSelectedFindingIDs preserves the old API for callers that do not
// need to distinguish how the selection was made.
func (d *DB) SetStepRoundSelectedFindingIDs(id string, selectedFindingIDs *string) error {
	return d.SetStepRoundSelection(id, selectedFindingIDs, RoundSelectionSourceUser)
}

// SetStepRoundUserFindings records the merged finding list (with user
// instructions attached and user-added findings appended) that was
// dispatched to the fix agent for the round. Passing nil clears the column.
func (d *DB) SetStepRoundUserFindings(id string, userFindingsJSON *string) error {
	if _, err := d.sql.Exec(
		`UPDATE step_rounds SET user_findings_json = ? WHERE id = ?`,
		userFindingsJSON, id,
	); err != nil {
		return fmt.Errorf("set step round user findings: %w", err)
	}
	return nil
}

// SetStepRoundGateAction records how the round's approval gate was answered:
// the action, who answered it (one of the GateActionSource constants), and a
// short reason. It is advisory provenance for status and report surfaces - the
// action itself is applied by the executor, not read back from here - so a
// write failure must never be treated as a reason to abandon the gate.
//
// An empty action clears all three columns, which keeps the column set
// consistent rather than leaving a source with no action beside it.
func (d *DB) SetStepRoundGateAction(id, action, source, reason string) error {
	var actionPtr, sourcePtr, reasonPtr *string
	if action != "" {
		actionPtr = &action
		if source != "" {
			sourcePtr = &source
		}
		if reason != "" {
			bounded := truncateGateActionReason(reason)
			reasonPtr = &bounded
		}
	}
	if _, err := d.sql.Exec(
		`UPDATE step_rounds SET gate_action = ?, gate_action_source = ?, gate_action_reason = ? WHERE id = ?`,
		actionPtr, sourcePtr, reasonPtr, id,
	); err != nil {
		return fmt.Errorf("set step round gate action: %w", err)
	}
	return nil
}

func truncateGateActionReason(reason string) string {
	if len(reason) <= maxGateActionReason {
		return reason
	}
	// Cut on a rune boundary so the stored reason stays valid UTF-8.
	cut := maxGateActionReason
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut] + "..."
}

// GetRoundsByStep returns all rounds for a step result, ordered by round number.
func (d *DB) GetRoundsByStep(stepResultID string) ([]*StepRound, error) {
	rows, err := d.sql.Query(
		`SELECT id, step_result_id, round, trigger_type, findings_json, reviewed_head_sha, starting_head_sha, trusted_config_sha, global_config_yaml, repo_config_yaml, user_findings_json, selected_finding_ids, selection_source, fix_summary, gate_action, gate_action_source, gate_action_reason, duration_ms, created_at FROM step_rounds WHERE step_result_id = ? ORDER BY round`,
		stepResultID,
	)
	if err != nil {
		return nil, fmt.Errorf("get rounds by step: %w", err)
	}
	defer rows.Close()
	var rounds []*StepRound
	for rows.Next() {
		r := &StepRound{}
		if err := rows.Scan(&r.ID, &r.StepResultID, &r.Round, &r.Trigger, &r.FindingsJSON, &r.ReviewedHeadSHA, &r.StartingHeadSHA, &r.TrustedConfigSHA, &r.GlobalConfigYAML, &r.RepoConfigYAML, &r.UserFindingsJSON, &r.SelectedFindingIDs, &r.SelectionSource, &r.FixSummary, &r.GateAction, &r.GateActionSource, &r.GateActionReason, &r.DurationMS, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan step round: %w", err)
		}
		rounds = append(rounds, r)
	}
	return rounds, rows.Err()
}
