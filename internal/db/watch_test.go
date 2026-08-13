package db

import "testing"

func insertWatchTestRepo(t *testing.T, d *DB) *Repo {
	t.Helper()
	repo, err := d.InsertConnectedRepo(
		"aabbccddeeff", "/nm/sources/aabbccddeeff",
		"https://example.test/acme/api.git", "example.test/acme/api", "acme-api", "main",
	)
	if err != nil {
		t.Fatalf("InsertConnectedRepo: %v", err)
	}
	return repo
}

func TestInsertConnectedRepoIsFoundByIdentityAndName(t *testing.T) {
	d := openTestDB(t)
	repo := insertWatchTestRepo(t, d)

	if !repo.Connected() {
		t.Fatal("InsertConnectedRepo produced a repo that does not report Connected()")
	}

	byIdentity, err := d.GetRepoBySourceIdentity("example.test/acme/api")
	if err != nil || byIdentity == nil {
		t.Fatalf("GetRepoBySourceIdentity = %v, %v", byIdentity, err)
	}
	if byIdentity.ID != repo.ID {
		t.Errorf("identity lookup returned %q, want %q", byIdentity.ID, repo.ID)
	}

	byName, err := d.GetRepoBySourceName("acme-api")
	if err != nil || byName == nil {
		t.Fatalf("GetRepoBySourceName = %v, %v", byName, err)
	}
	if byName.ID != repo.ID {
		t.Errorf("name lookup returned %q, want %q", byName.ID, repo.ID)
	}

	// An empty selector must not match a row whose column is empty.
	if got, err := d.GetRepoBySourceIdentity(""); err != nil || got != nil {
		t.Errorf("GetRepoBySourceIdentity(\"\") = %v, %v; want nil, nil", got, err)
	}
	if got, err := d.GetRepoBySourceName(""); err != nil || got != nil {
		t.Errorf("GetRepoBySourceName(\"\") = %v, %v; want nil, nil", got, err)
	}
}

// TestSetRepoDetachedAtIsReversibleAndPreservesTheRecord pins that detaching is
// not deletion: the row, its ID, and therefore its gate and run history survive,
// so re-adding the same URL later reuses all of it.
func TestSetRepoDetachedAtIsReversibleAndPreservesTheRecord(t *testing.T) {
	d := openTestDB(t)
	repo := insertWatchTestRepo(t, d)

	stamp := int64(12345)
	if err := d.SetRepoDetachedAt(repo.ID, &stamp); err != nil {
		t.Fatalf("SetRepoDetachedAt: %v", err)
	}
	detached, err := d.GetRepo(repo.ID)
	if err != nil || detached == nil {
		t.Fatalf("GetRepo after detach = %v, %v", detached, err)
	}
	if detached.DetachedAt == nil || *detached.DetachedAt != stamp {
		t.Fatalf("DetachedAt = %v, want %d", detached.DetachedAt, stamp)
	}
	if detached.SourceIdentity != "example.test/acme/api" {
		t.Error("detaching lost the source identity; re-adding the URL would create a second gate")
	}

	if err := d.SetRepoDetachedAt(repo.ID, nil); err != nil {
		t.Fatalf("SetRepoDetachedAt(nil): %v", err)
	}
	reattached, err := d.GetRepo(repo.ID)
	if err != nil || reattached == nil {
		t.Fatalf("GetRepo after reattach = %v, %v", reattached, err)
	}
	if reattached.DetachedAt != nil {
		t.Errorf("DetachedAt = %v after clearing, want nil", reattached.DetachedAt)
	}
}

func TestUpdateConnectedRepoSpecClearsDetachedAndLeavesURLsAlone(t *testing.T) {
	d := openTestDB(t)
	repo := insertWatchTestRepo(t, d)
	stamp := int64(999)
	if err := d.SetRepoDetachedAt(repo.ID, &stamp); err != nil {
		t.Fatalf("SetRepoDetachedAt: %v", err)
	}

	updated, err := d.UpdateConnectedRepoSpec(repo.ID, "acme-api-renamed", "trunk")
	if err != nil {
		t.Fatalf("UpdateConnectedRepoSpec: %v", err)
	}
	if updated.SourceName != "acme-api-renamed" {
		t.Errorf("SourceName = %q", updated.SourceName)
	}
	if updated.DefaultBranch != "trunk" {
		t.Errorf("DefaultBranch = %q", updated.DefaultBranch)
	}
	if updated.DetachedAt != nil {
		t.Error("UpdateConnectedRepoSpec did not clear detached_at")
	}
	// The redaction invariant lives on upstream_url; only the URL refresh path
	// may touch it, so a spec update must leave it exactly as it was.
	if updated.UpstreamURL != repo.UpstreamURL {
		t.Errorf("UpstreamURL changed to %q, want %q: only the URL refresh path may write it", updated.UpstreamURL, repo.UpstreamURL)
	}
	// Renaming preserves the ID, and with it the gate and run history.
	if updated.ID != repo.ID {
		t.Errorf("ID changed on rename: %q -> %q", repo.ID, updated.ID)
	}
}

func TestWatchRoundTripAndCascade(t *testing.T) {
	d := openTestDB(t)
	repo := insertWatchTestRepo(t, d)

	interval := int64(900)
	w, err := d.InsertWatch(repo.ID, WatchKindBranch, "release/*", "report", &interval, "02:30")
	if err != nil {
		t.Fatalf("InsertWatch: %v", err)
	}

	got, err := d.GetWatch(w.ID)
	if err != nil || got == nil {
		t.Fatalf("GetWatch = %v, %v", got, err)
	}
	if got.Pattern != "release/*" || got.Mode != "report" || got.Kind != WatchKindBranch {
		t.Errorf("watch round-trip mismatch: %+v", got)
	}
	if !got.Enabled {
		t.Error("a newly registered watch is not enabled")
	}
	if got.PollIntervalSeconds == nil || *got.PollIntervalSeconds != 900 {
		t.Errorf("PollIntervalSeconds = %v, want 900", got.PollIntervalSeconds)
	}
	if got.DailyAt != "02:30" {
		t.Errorf("DailyAt = %q", got.DailyAt)
	}

	// A nil interval must read back as nil, not as zero: "use the global
	// default" and "poll every zero seconds" are different instructions.
	w2, err := d.InsertWatch(repo.ID, WatchKindPR, "", "comment", nil, "")
	if err != nil {
		t.Fatalf("InsertWatch pr: %v", err)
	}
	got2, err := d.GetWatch(w2.ID)
	if err != nil || got2 == nil {
		t.Fatalf("GetWatch pr = %v, %v", got2, err)
	}
	if got2.PollIntervalSeconds != nil {
		t.Errorf("PollIntervalSeconds = %v, want nil for an unset override", *got2.PollIntervalSeconds)
	}

	byRepo, err := d.GetWatchesByRepo(repo.ID)
	if err != nil {
		t.Fatalf("GetWatchesByRepo: %v", err)
	}
	if len(byRepo) != 2 {
		t.Fatalf("GetWatchesByRepo returned %d, want 2", len(byRepo))
	}

	if err := d.SetWatchEnabled(w.ID, false); err != nil {
		t.Fatalf("SetWatchEnabled: %v", err)
	}
	disabled, err := d.GetWatch(w.ID)
	if err != nil || disabled == nil {
		t.Fatalf("GetWatch after disable = %v, %v", disabled, err)
	}
	if disabled.Enabled {
		t.Error("watch still enabled after SetWatchEnabled(false)")
	}

	// Deleting the repository must cascade through watches to their state.
	if err := d.AdvanceWatchState(w.ID, "refs/heads/main", "sha1", "run-1", "completed"); err != nil {
		t.Fatalf("AdvanceWatchState: %v", err)
	}
	if err := d.DeleteRepo(repo.ID); err != nil {
		t.Fatalf("DeleteRepo: %v", err)
	}
	remaining, err := d.GetWatches()
	if err != nil {
		t.Fatalf("GetWatches: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("watches survived repo deletion: %d", len(remaining))
	}
	var stateRows int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM watch_state`).Scan(&stateRows); err != nil {
		t.Fatalf("count watch_state: %v", err)
	}
	if stateRows != 0 {
		t.Errorf("watch_state rows survived repo deletion: %d", stateRows)
	}
}

func TestInsertWatchRejectsAnExactDuplicate(t *testing.T) {
	d := openTestDB(t)
	repo := insertWatchTestRepo(t, d)

	if _, err := d.InsertWatch(repo.ID, WatchKindBranch, "main", "report", nil, ""); err != nil {
		t.Fatalf("first InsertWatch: %v", err)
	}
	if _, err := d.InsertWatch(repo.ID, WatchKindBranch, "main", "report", nil, ""); err == nil {
		t.Fatal("duplicate watch accepted; every ref it matches would be validated twice per sweep")
	}
	// The same pattern in a different mode is a legitimately different watch.
	if _, err := d.InsertWatch(repo.ID, WatchKindBranch, "main", "comment", nil, ""); err != nil {
		t.Fatalf("same pattern in a different mode was rejected: %v", err)
	}
}

// TestRecordWatchFailureDoesNotAdvanceLastSeenSHA is the crash-safety
// invariant. If a failure advanced the SHA, work that was never validated would
// be silently retired and never retried.
func TestRecordWatchFailureDoesNotAdvanceLastSeenSHA(t *testing.T) {
	d := openTestDB(t)
	repo := insertWatchTestRepo(t, d)
	w, err := d.InsertWatch(repo.ID, WatchKindBranch, "main", "report", nil, "")
	if err != nil {
		t.Fatalf("InsertWatch: %v", err)
	}

	if err := d.AdvanceWatchState(w.ID, "refs/heads/main", "sha-validated", "run-1", "completed"); err != nil {
		t.Fatalf("AdvanceWatchState: %v", err)
	}
	if err := d.RecordWatchFailure(w.ID, "refs/heads/main", "run-2", "failed"); err != nil {
		t.Fatalf("RecordWatchFailure: %v", err)
	}

	states, err := d.GetWatchState(w.ID)
	if err != nil {
		t.Fatalf("GetWatchState: %v", err)
	}
	st := states["refs/heads/main"]
	if st == nil {
		t.Fatal("no state for refs/heads/main")
	}
	if st.LastSeenSHA != "sha-validated" {
		t.Errorf("LastSeenSHA = %q, want the last SUCCESSFULLY validated sha; a failure must leave the work outstanding", st.LastSeenSHA)
	}
	if st.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", st.ConsecutiveFailures)
	}
	if st.LastStatus != "failed" {
		t.Errorf("LastStatus = %q, want failed", st.LastStatus)
	}

	// Streaks accumulate, which is what a caller backs off on.
	if err := d.RecordWatchFailure(w.ID, "refs/heads/main", "run-3", "failed"); err != nil {
		t.Fatalf("second RecordWatchFailure: %v", err)
	}
	states, _ = d.GetWatchState(w.ID)
	if states["refs/heads/main"].ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2", states["refs/heads/main"].ConsecutiveFailures)
	}

	// A later success clears the streak.
	if err := d.AdvanceWatchState(w.ID, "refs/heads/main", "sha-new", "run-4", "completed"); err != nil {
		t.Fatalf("AdvanceWatchState after failures: %v", err)
	}
	states, _ = d.GetWatchState(w.ID)
	if got := states["refs/heads/main"]; got.ConsecutiveFailures != 0 || got.LastSeenSHA != "sha-new" {
		t.Errorf("after success: failures=%d sha=%q, want 0 and sha-new", got.ConsecutiveFailures, got.LastSeenSHA)
	}
}

// TestRecordWatchFailureOnAnUnseenRefDoesNotInventASHA pins that the first
// observation of a ref failing leaves an empty LastSeenSHA rather than a
// fabricated one, so the ref is still treated as never validated.
func TestRecordWatchFailureOnAnUnseenRefDoesNotInventASHA(t *testing.T) {
	d := openTestDB(t)
	repo := insertWatchTestRepo(t, d)
	w, err := d.InsertWatch(repo.ID, WatchKindBranch, "main", "report", nil, "")
	if err != nil {
		t.Fatalf("InsertWatch: %v", err)
	}
	if err := d.RecordWatchFailure(w.ID, "refs/heads/new", "run-1", "failed"); err != nil {
		t.Fatalf("RecordWatchFailure: %v", err)
	}
	states, err := d.GetWatchState(w.ID)
	if err != nil {
		t.Fatalf("GetWatchState: %v", err)
	}
	st := states["refs/heads/new"]
	if st == nil {
		t.Fatal("no state recorded for the failed ref")
	}
	if st.LastSeenSHA != "" {
		t.Errorf("LastSeenSHA = %q, want empty: a ref that never validated must not look validated", st.LastSeenSHA)
	}
}
