package lifecycle

import (
	"errors"
	"fmt"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ActiveRuns returns all pending/running pipeline runs from the local state DB.
func ActiveRuns(p *paths.Paths) ([]*db.Run, error) {
	if p == nil {
		return nil, nil
	}
	dbPath := p.DB()
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat database: %w", err)
	}

	database, err := db.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer database.Close()
	return database.GetActiveRuns()
}

func RunList(runs []*db.Run) string {
	if len(runs) == 0 {
		return ""
	}
	out := "active pipeline runs:\n"
	for _, run := range runs {
		out += fmt.Sprintf("  %s  %s  %s  %s\n", run.ID, run.Status, run.Branch, ShortSHA(run.HeadSHA))
	}
	return out
}

func ShortSHA(sha string) string {
	if len(sha) <= 8 {
		return sha
	}
	return sha[:8]
}

// BlockingRuns returns the active runs a destructive lifecycle operation must
// actually protect.
//
// ActiveRuns keeps returning everything - that is the honest answer to "what is
// running" - while this classifies what stopping the daemon would genuinely
// cost. The distinction exists because a nightly sweep would otherwise make
// `daemon restart` require --force most nights, training operators to always
// pass it and defeating the guard for the work it was built for.
//
// A read-only QA run never pushed and never opened a PR, so cancelling it costs
// tokens and the next sweep re-derives it from watch_state. A fix-pr QA run
// DOES block: "the tool must not lose people's code" draws no distinction
// between a gate run's commits and a QA run's.
func BlockingRuns(runs []*db.Run) []*db.Run {
	var blocking []*db.Run
	for _, run := range runs {
		if runBlocksLifecycle(run) {
			blocking = append(blocking, run)
		}
	}
	return blocking
}

// CancellableRuns is the exact complement of BlockingRuns, so a guard can
// report "also cancelling 3 QA runs" rather than discarding them silently.
func CancellableRuns(runs []*db.Run) []*db.Run {
	var cancellable []*db.Run
	for _, run := range runs {
		if !runBlocksLifecycle(run) {
			cancellable = append(cancellable, run)
		}
	}
	return cancellable
}

// runBlocksLifecycle is the single owner of the rule. It fails SAFE: anything
// that is not a recognized read-only QA run blocks, so an unknown or malformed
// kind is protected rather than discarded.
func runBlocksLifecycle(run *db.Run) bool {
	if run == nil {
		return false
	}
	if types.NormalizeRunKind(run.RunKind) != types.RunKindQA {
		return true
	}
	return types.NormalizeRunMode(run.RunMode).WritesCode()
}

// CancellableRunList renders the runs a destructive operation would discard.
func CancellableRunList(runs []*db.Run) string {
	if len(runs) == 0 {
		return ""
	}
	out := "read-only QA runs that will be cancelled (the next sweep re-derives them):\n"
	for _, run := range runs {
		out += fmt.Sprintf("  %s  %s  %s\n", run.ID, run.Status, run.Branch)
	}
	return out
}
