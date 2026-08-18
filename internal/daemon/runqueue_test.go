package daemon

import (
	"sync"
	"testing"
)

// TestBranchLocksDoNotGrowUnbounded pins the reason branchLockSet replaced a
// bare sync.Map: the map never removed a key, which a QA sweep over many
// repositories times many refs would grow without ceiling in a long-lived
// daemon.
func TestBranchLocksDoNotGrowUnbounded(t *testing.T) {
	locks := newBranchLockSet()
	for i := 0; i < 1000; i++ {
		release := locks.Acquire(string(rune('a'+i%26)) + string(rune(i)))
		release()
	}
	if got := locks.len(); got != 0 {
		t.Errorf("%d entries retained after every holder released, want 0", got)
	}
}

// TestBranchLockStillSerializesConcurrentStartsForSameBranch pins that
// refcounting did not weaken the mutual exclusion the locks exist for: two
// concurrent starts on one branch must not interleave.
func TestBranchLockStillSerializesConcurrentStartsForSameBranch(t *testing.T) {
	locks := newBranchLockSet()
	var mu sync.Mutex
	inside, maxInside := 0, 0

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := locks.Acquire("repo/main")
			defer release()
			mu.Lock()
			inside++
			if inside > maxInside {
				maxInside = inside
			}
			mu.Unlock()
			mu.Lock()
			inside--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Errorf("max concurrent holders = %d, want 1", maxInside)
	}
	if got := locks.len(); got != 0 {
		t.Errorf("%d entries retained, want 0", got)
	}
}

// TestBranchLocksAreIndependentAcrossBranches guards against over-serializing:
// different branches must proceed in parallel.
func TestBranchLocksAreIndependentAcrossBranches(t *testing.T) {
	locks := newBranchLockSet()
	first := locks.Acquire("repo/main")
	done := make(chan struct{})
	go func() {
		release := locks.Acquire("repo/other")
		release()
		close(done)
	}()
	<-done // would deadlock if a different branch blocked on the held lock
	first()
}

// TestQARunConcurrencyCapNeverDelaysAPush is the product promise in test form:
// "validation must not hold the author hostage". Interactive work is counted
// against the ceiling QA respects, but is never itself refused by it.
func TestQARunConcurrencyCapNeverDelaysAPush(t *testing.T) {
	slots := newRunSlots(1, 1)

	// Saturate with QA work.
	releaseQA, ok := slots.TryAcquireQA()
	if !ok {
		t.Fatal("first QA acquire was refused on an empty pool")
	}
	if _, ok := slots.TryAcquireQA(); ok {
		t.Error("QA exceeded max_concurrent")
	}

	// A push still starts immediately, even past the ceiling.
	for i := 0; i < 5; i++ {
		if release := slots.AcquireInteractive(); release == nil {
			t.Fatal("interactive acquire was refused; a push must never be gated")
		}
	}
	interactive, qa := slots.counts()
	if interactive != 5 || qa != 1 {
		t.Errorf("counts = interactive %d, qa %d; want 5 and 1", interactive, qa)
	}

	// With the ceiling exceeded by interactive work, QA stops admitting.
	releaseQA()
	if _, ok := slots.TryAcquireQA(); ok {
		t.Error("QA was admitted while interactive work exceeded max_total_runs")
	}
}

// TestRunSlotsAdmitQAAgainAfterInteractiveWorkDrains pins that the ceiling is a
// transient overshoot rather than a permanent lockout.
func TestRunSlotsAdmitQAAgainAfterInteractiveWorkDrains(t *testing.T) {
	slots := newRunSlots(2, 2)
	release := slots.AcquireInteractive()
	releaseTwo := slots.AcquireInteractive()
	if _, ok := slots.TryAcquireQA(); ok {
		t.Fatal("QA admitted while the ceiling was full of interactive work")
	}
	release()
	releaseTwo()
	if _, ok := slots.TryAcquireQA(); !ok {
		t.Error("QA was not admitted after interactive work drained")
	}
}
