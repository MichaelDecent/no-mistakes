package daemon

import (
	"fmt"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// branchLockSet serializes run starts per repository+branch with refcounted
// entries that are deleted when their last holder releases.
//
// It replaces a bare sync.Map, which never removed a key. That was bounded in
// practice by how often a human pushes, but a QA sweep over many repositories
// times many refs makes the key set grow without any ceiling, and the daemon is
// a long-lived service.
//
// Correctness rests on incrementing the refcount under the SET mutex before
// taking the per-key mutex. A concurrent release therefore cannot delete an
// entry that a waiter is about to block on, which is the classic bug in
// refcounted lock maps.
type branchLockSet struct {
	mu      sync.Mutex
	entries map[string]*branchLockEntry
}

type branchLockEntry struct {
	mu   sync.Mutex
	refs int
}

func newBranchLockSet() *branchLockSet {
	return &branchLockSet{entries: map[string]*branchLockEntry{}}
}

// Acquire locks the named branch and returns the release function. The returned
// function must be called exactly once.
func (s *branchLockSet) Acquire(key string) func() {
	s.mu.Lock()
	entry, ok := s.entries[key]
	if !ok {
		entry = &branchLockEntry{}
		s.entries[key] = entry
	}
	// Claim the entry BEFORE releasing the set mutex, so a concurrent release
	// cannot delete what this caller is about to block on.
	entry.refs++
	s.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		s.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(s.entries, key)
		}
		s.mu.Unlock()
	}
}

// len reports the number of live entries. Test-only observability for the
// unbounded-growth regression.
func (s *branchLockSet) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// runSlots bounds how much work runs at once.
//
// The asymmetry is the whole design. An INTERACTIVE run - a push, a rerun, a
// crash-recovery resume - is never rejected and never queued, because
// "validation must not hold the author hostage" is the product's core promise.
// Interactive runs COUNT toward the total but are not GATED by it, so a burst of
// pushes transiently exceeds the ceiling and QA simply stops admitting until it
// drains.
//
// QA work is capped and yields. A rejected QA item is re-derived by the next
// sweep from watch_state, so declining is lossless in a way declining a push
// would not be.
//
// The defaults are reasoned, not measured: each run spawns agent CLI
// subprocesses that routinely hold 1-3 GB, every database write serializes on a
// single connection, and this daemon has already been OOM-killed in the field by
// leaked grandchildren. A new source of concurrency starts conservative.
type runSlots struct {
	mu            sync.Mutex
	interactive   int
	qa            int
	maxConcurrent int
	maxTotal      int
}

func newRunSlots(maxConcurrent, maxTotal int) *runSlots {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	if maxTotal <= 0 {
		maxTotal = maxConcurrent
	}
	return &runSlots{maxConcurrent: maxConcurrent, maxTotal: maxTotal}
}

// AcquireInteractive always succeeds. It exists so interactive work is counted
// against the ceiling QA respects, never so it can be refused.
func (s *runSlots) AcquireInteractive() func() {
	s.mu.Lock()
	s.interactive++
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.interactive--
		s.mu.Unlock()
	}
}

// TryAcquireQA admits a QA run only when both the QA cap and the overall
// ceiling allow it. It never blocks: the caller queues or re-derives instead.
func (s *runSlots) TryAcquireQA() (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.qa >= s.maxConcurrent || s.interactive+s.qa >= s.maxTotal {
		return nil, false
	}
	s.qa++
	return func() {
		s.mu.Lock()
		s.qa--
		s.mu.Unlock()
	}, true
}

// counts reports live occupancy for `qa status` and tests.
func (s *runSlots) counts() (interactive, qa int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interactive, s.qa
}

// activeGateRunForBranch returns the ID of an active gate-kind run on the
// branch, or "" when none exists. Callers hold the branch lock, so the answer
// cannot race a concurrent start on the same branch.
func (m *RunManager) activeGateRunForBranch(repoID, branch string) (string, error) {
	runs, err := m.db.GetActiveRuns()
	if err != nil {
		return "", fmt.Errorf("list active runs: %w", err)
	}
	for _, run := range runs {
		if run.RepoID != repoID || run.Branch != branch {
			continue
		}
		if types.NormalizeRunKind(run.RunKind) == types.RunKindGate {
			return run.ID, nil
		}
	}
	return "", nil
}
