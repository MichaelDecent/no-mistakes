# QA Agent Plan — working document

> **This is a temporary working document tracking PR #1**
> (`claude/qa-agent-multi-repo-plan-siv96n`). It exists so a new session can pick the work up mid-stream.
> **Delete it in a final commit before this branch merges.**
>
> Specs S1–S15 are complete and pushed. **Start at S16.**

---

# Extend `no-mistakes` into a multi-repo QA agent


## Execution status

**PR: <https://github.com/MichaelDecent/no-mistakes/pull/1>**, branch
`claude/qa-agent-multi-repo-plan-siv96n`, base `main`. Do **not** open another PR; pushing to this
branch updates it. Pushing works — the earlier `403` is resolved.

**⚠ GitHub Actions is disabled on this fork, so PR #1 has NO automated verification.**
`list_workflows` returns `total_count: 0` and the PR reports zero check runs with a `pending` combined
status, even though `.github/workflows/ci.yml` is present in the tree and its `pull_request` trigger
matches `main`. This is the default for a newly created fork — the workflows exist but are not
registered until the owner enables Actions (repo **Settings → Actions → General**, or the "I understand
my workflows, go ahead and enable them" banner on the Actions tab).

Consequence to carry: the claim that the eight failures below are environmental rests **only** on the
stash-and-rerun comparison in this container. Nothing has verified it on Linux CI, and until Actions is
enabled nothing will — including the `go test -race ./...` broad-regression leg that
`AGENTS.md` designates as the owner of full regression coverage.

Three corrections landed during Phase 1 and are already reflected below: `rebase` and `intent` are
NOT skipped in QA modes (verified: `rebase.go` has zero `git.Push*` calls and aborts on conflict;
`IsAuthoritativeRunIntentSource` is a two-value whitelist so a commit-derived source is
non-authoritative by construction), and `trigger` is safe as a SQLite column name (tested
CREATE/INSERT/SELECT against `modernc.org/sqlite`).

**The `SourceYAML` credential spill is also fixed** — `65ac9e5`, the second local commit. That was the
one ship-blocking item, so it deliberately landed on its own rather than inside a larger phase.

**STAGES A, B, AND C ARE COMPLETE, AND STAGE D IS UNDERWAY — S1–S15 of 23, all pushed to PR #1.**

| Spec | Commit |
|---|---|
| S8 connected gate provisioning | `f566cd0` |
| S9 URL refresh guard + C1 binding | `032d4a3` |
| S10 reconciliation + `repos` CLI | `6473ac6` |
| S11 run-start fails closed on gate drift | `f3b45c8` |
| S12 run slots + self-pruning branch locks | `98f424d` |
| S13 lifecycle blocking classification | `5d84391` |
| S14 `applyApprovalAction` seam | `3d28d90` |
| S15 `GatePolicy` seam + both policies | `0afa116` |

**Next: S16** (skip sets + commit-derived intent), then S17–S18 to reach the first useful milestone:
`qa run --mode report` producing a real report.

**What S15 hands S16.** The seam is complete and installed, but **nothing creates a QA run yet**, so every
policy path is still unreachable in production:

- `internal/pipeline/gatepolicy.go` owns `GateRequest`, `GateDecision`, `GatePolicy`, `SetGatePolicy`, and
  the single consult helper `resolveGateByPolicy`, called from `executeStep` and from `Resume`.
- `internal/qa/policy.go` owns `PolicyFor(kind, mode)` plus `readOnlyGatePolicy` and
  `convergingGatePolicy`. `PolicyFor` is the only place a kind/mode pair becomes a policy.
- `internal/daemon/manager.go` `gatePolicyForRun(run)` reads the run row through
  `types.NormalizeRunKind`/`NormalizeRunMode` and is called on **both** the start path (next to
  `SetSkippedSteps`, inert today because every started run is a gate run) and the recovery path. S16/S17
  therefore only have to write the right `run_kind`/`run_mode`; no further wiring is needed to make gates
  resolve.
- `runs.skipped_steps` is still unread — that is S16 — and `db.StepRound` now carries
  `GateAction`/`GateActionSource`/`GateActionReason`, which S18's report can read as gate provenance.

### Deviations recorded while implementing Stage B/C

1. **The duplicate-name check moved twice** — planned for S4, reassigned to S8, and finally landed in
   **S10's `ReconcileConnected`**. Only a caller holding the whole spec set can distinguish "two names for
   one repository" from a *rename*, and renaming is explicitly supported because it preserves the gate and
   run history. `EnsureConnected` refuses only what it can decide alone.
2. **A connected repo's URL must be scheme-qualified or `host:path`.** A filesystem path and `file://`
   both resolve to `invalid remote`. See the S8 spec section for the three options when this bites at
   S19/e2e.
3. **`db.Run` had no `RunKind`/`RunMode` fields** — S1 added the columns but never the struct fields or
   the scan, so a run's kind could not be read at all. Closed in S12.
4. **S12's QA-never-supersedes-gate refusal is implemented but not wired.**
   `activeGateRunForBranch` exists and is correct; the call site needs a run-kind parameter on
   `startRunWithIntentSource` that only arrives with S16/S17. **Wire it there** — this is the one piece of
   deliberately unfinished business carried forward.

### Deviations recorded while implementing S15

1. **The policy consult sits AFTER the execution-timer freeze, not before it.** The design snippet put it
   before, but `applyApprovalAction` restarts the phase clock, so resolving before
   `executionMS += time.Since(phaseStart)` discards the round's own execution time and reports every
   unattended step as instant. The freeze is local arithmetic that nothing can observe, so it is not a park
   side effect; every actual park side effect (`ParkStepForApproval`, `e.waiting`, the wait) still happens
   strictly after the consult.
2. **Cancellation outranks a policy.** `resolveGateByPolicy` declines when the run context is already
   cancelled, so a run being stopped (shutdown, supersede, abort, `max_run_duration`) cannot be advanced
   into push/PR/CI by an unattended approval. Regression:
   `TestGatePolicyDoesNotResolveAGateAfterCancellation`.
3. **A policy's decisions are attributed to the policy, not to a user.** `applyApprovalAction` gained an
   `actionSource`, `db.RoundSelectionSourcePolicy` was added, and the `approval` telemetry event — which
   describes a person answering a gate — is not emitted for a policy decision on either the live or the
   recovery path. Without this, an unattended fix round records as a human's selection.
4. **`recoveredRunPlan` needed no new fields.** It already carries the `runs` row, so `gatePolicyForRun`
   reads kind and mode from `plan.run` instead. That helper is also called on the normal start path, where
   it is inert (every started run is a gate run, and a gate run always resolves to a nil policy), so both
   paths share one owner rather than two spellings of the same decision.
5. **`PolicyFor` returns nil for an unrecognized QA mode** rather than guessing a policy. Nil means the gate
   parks, which can never write code or approve anything; the run then stalls into the watcher's
   `max_run_duration`. Unreachable in practice because `NormalizeRunMode` resolves a stored value first.
6. **`TestRecoveredQARunReinstallsItsGatePolicyAndSkipSet` split in two**, because the skip-set half belongs
   to S16: `TestResumePolicyResolvesARecoveredGateWithoutWaiting` (pipeline) covers the recovered-gate
   resolution, and `TestGatePolicyForRunIsNilForEveryGateRun` /
   `TestGatePolicyForRunInstallsAPolicyForQARuns` (daemon) cover what recovery installs.
7. **S1's three `step_rounds` gate-action columns got their first Go API here** — struct fields, the SELECT
   list, `SetStepRoundGateAction`, and the `GateActionSource*` vocabulary — with the reason bounded at the
   persistence boundary so no future caller can turn the column into a payload sink.

`docs/src/content/docs/concepts/qa.md` gained the "Nobody is there to answer a gate" section; it is the
owner of that model fact.

---

**Stage A detail.** S1–S7 committed and pushed:

| Spec | Commit | Notes |
|---|---|---|
| S1 inert foundations | `cac0974` | |
| S2 `SourceYAML` redaction | `65ac9e5` | the one ship-blocking item, landed alone |
| S3 author surfaces refuse connected | `ce02f08` | |
| S4+S5 `repos:` / `qa:` config | `c62895d` | landed together — same file, same precedent, same test pass |
| S6 operator project-instruction floor | `eb33554` | |
| S7 `VISION.md` + `concepts/qa.md` | `71858f9` | |

**Next: S8** — connected gate provisioning, the first spec that can create a connected repository. Every
guard it needs is now in place.

**Three verified corrections to the design, already reflected below:**

1. §8 claimed `findRepo` covers `init`, `eject`, and `stats`. It does **not** — `init`/`eject` reach
   `gate.go` with a `workDir` and never call it, and `stats` aggregates every repo. The `gate.go` and
   `stats.go` changes are load-bearing, not cleanup.
2. `gate.InitWithFork` had **no** guard against roots under `ReposDir()`/`WorktreesDir()`. That
   pre-existing gap is now closed by `refuseDaemonOwnedRoot`.
3. **S4 does not resolve remote identities**, contrary to §6. `gate.RemoteIdentity` is the single owner of
   that normalization, and importing `internal/gate` into `internal/config` would pull the agent, db, and
   scm trees into the dependency graph of nearly every package. Config validates only what it can decide
   alone (name charset, url present and free of whitespace/control characters — a strict subset of what
   the normalizer accepts, so the two cannot disagree). **Identity resolution and duplicate-identity
   refusal move to S8/S10**, where the live DB is available and where a collision can actually do harm.

The work is sliced into 23 single-PR specs (see [Specs](#specs)); S1–S3 are the three commits above.

### Pre-existing test failures in this container (not caused by this work)

Five tests fail identically on a clean checkout of `origin/main`, all in the process-group
reaping/timeout family that this sandbox does not support, plus one cwd-dependent test. Verified by
stashing and re-running. Do not chase them as regressions:
`TestCodexAgent_Run_ReapsGrandchildHoldingStdoutPipeOnLeaderExit`,
`TestRunShellCommandWithEnv_KillsGrandchildOnCancel`, `TestDefaultShellCommandOutput_TimesOut`,
`TestDefaultShellCommandOutput_TimesOutWithPipeHoldingChild`, `TestPRStep_GhNotAvailable`, and e2e
`TestDaemonStopLeavesNoDaemonProcessOwningTheRoot` / `TestDaemonRestartReplacesTheDaemonWithExactlyOneOwner`.
`TestCIStep_CIAutoFixDisabledWithZero` is separately flaky under parallel load and passes in isolation.

## Context

`no-mistakes` today is an **author-side pre-push gate for one local clone at a time**. The operator
runs `no-mistakes init` in their own checkout; that creates a bare "gate" repo at
`<NM_HOME>/repos/<repoID>.git` and adds a git remote named `no-mistakes` to the clone. The operator
runs `git push no-mistakes <branch>`; a `post-receive` hook notifies a singleton daemon, which carves
a disposable worktree from the bare gate and runs a fixed nine-step pipeline
(`intent → rebase → review → test → document → lint → push → pr → ci`), then force-pushes to the real
remote and opens a PR. Every approval gate **blocks indefinitely** until a human or driving agent
answers over IPC.

The goal is a **QA agent over several repositories connected declaratively by remote URL**, where the
daemon owns the registration and no developer checkout exists. Confirmed requirements:

- **Triggers:** poll for new commits on watched branches and for new/updated PRs; a scheduled sweep;
  manual on-demand per repo. The existing push trigger stays untouched.
- **Output mode, per repo:** `report` (read-only), `comment` (post findings to the forge), `fix-pr`
  (today's full behaviour).
- **Connection model:** declarative remote URLs in operator-owned config.
- **Pipeline:** reuse the existing pipeline; express each mode as a per-run skip set.
- **Scale:** 2–10 repositories.
- **Unattended writes: out of scope.** Unattended runs never write code. `fix-pr` is manual-only.
- **Destination:** the operator's own fork; `VISION.md` and the docs site may be amended.

Vocabulary: a repo connected by URL is **connected** (`source_kind='connected'`); today's is **local**.
Do **not** use *managed* — it already means "a no-mistakes-owned bare gate" in
`gatecontext.registeredManagedCommonDir`, `git.RefreshManagedGateHooks`, and `managed-server.log`, and
every repo has one.

### Why this is a second product mode, not a feature

`VISION.md` is load-bearing and five clauses resist this directly:

| `VISION.md` | Conflict |
|---|---|
| "owns exactly one thing: the gate between a local branch and the configured push target" (L5) | QA watches remote refs; there is no local branch. |
| "runs happen on their machine, under their identity, **at their initiative**" (L64) | Polling and schedules are not per-run initiative. |
| "It is **not a CI system**" (L65) | A scheduled sweep that comments on PRs is adjacent to CI. |
| resist growing "into an **always-on service** the user did not ask for" (L70) | A watcher loop is exactly that — though here the operator *is* asking. |
| "a standing rule may never skip [steps] on anyone's behalf" (L13) | Per-repo modes are standing skip sets. |

Also L10: "Pushing through the gate is the consent boundary… nothing else implies that consent." A QA
run has no push, so it needs its **own narrower consent boundary** — which is why unattended modes must
not write code.

**Amending `VISION.md` is a Phase 2 deliverable.** It should state the product has two scopes — the
*gate* (unchanged; author-initiated; consent by push) and *QA* (operator-configured; consent by
declarative config; never writes code unless a human is present) — and that gate invariants are never
relaxed to serve QA.

**How the L13 skip tension is resolved structurally, not documentarily:**
1. `SkipSetFor(kind, mode)` returns `nil` for `RunKindGate` **regardless of mode** — configuration
   cannot reach a gate run's step set at all.
2. `HandlePushReceived` never consults QA policy: an author's push is always
   `kind=gate, mode=fix-pr, skips=<explicit --skip only>`. A repo cannot be configured so that pushing
   to it yields a weaker gate.
3. **The skip set is not configurable — only the mode is.** Three named, code-owned, reviewed profiles.
   There is no `qa.skip_steps` key and there must never be one.
4. `concepts/pipeline.md`'s "always all nine" stays literally true: **skipping is a step status, not a
   shorter step list.** All nine `step_results` rows are inserted for every run (`executor.go:168`) and
   `e.steps` is always `steps.AllSteps()` — which is also what keeps `recoveredGate`'s positional check
   working.

What remains genuinely unresolved: a `fix-pr` QA run pushes and opens PRs on a branch its operator did
not author. That is why it is **manual-trigger-only and never schedulable** in this plan.

---

## Verified findings that shape the design

Each was confirmed by reading (or running) the code. Several corrected my first design.

1. **BLOCKING — declarative config would spill credentials into SQLite and the eval corpus.**
   `cfg.SourceYAML = append([]byte(nil), data...)` (`config.go:1216`) keeps the **verbatim bytes of
   `config.yaml`**; `EnableEvalProvenance` copies them to `ReplayGlobalYAML` (`config.go:1814`);
   `executor.go:780` writes that into `step_rounds.global_config_yaml` on **every review round**; and
   `eval.Capture` copies cases under `<NM_HOME>/eval`. `evalDefaults()` is
   `{CaptureProvenance: true, AutoCapture: true}` — **on by default**. A token in a connected repo's
   URL would be persisted verbatim, repeatedly. This regression is introduced by this feature and must
   be fixed in the same phase that adds the config block.
2. **`working_path` must be an identity stub, not the gate dir.** It is `NOT NULL UNIQUE`, and SQLite
   cannot drop NOT NULL under this repo's additive-only, version-table-free migration style — a table
   rebuild that crashed midway, with `ON DELETE CASCADE` on `runs`, is a plausible history-loss path.
   **Pointing it at the gate is unsound:** `gate.RefreshRepoURLs` runs on *every* `startRun` and would
   read the gate's `origin`, which is by design the **full credentialled URL**, and `ReplaceRepoURLs`
   would persist it — destroying redaction in one run. A ~50 KB stub at `<NM_HOME>/sources/<repoID>`
   (`git init`; **no remotes, no refs, no objects**; only `user.name`/`user.email`) is fail-safe:
   `GetConfiguredRemoteURLs(stub, "origin")` errors, so local discovery can never overwrite the config
   authority even if a guard were missed.
3. **The stub must pin a commit identity or registration must fail.** `git.CopyLocalUserIdentity`
   (`git.go:570`) reads `--local user.name`/`user.email` and `continue`s on empty. With no identity,
   commits silently fall back to the daemon's *global* git identity, which on a headless host often
   does not exist — an opaque failure deep in a fix-commit path.
4. **`git fetch` cannot fire the receive hooks** — only `git-receive-pack` (a push) does, and no
   fetch-side or `reference-transaction` hook is installed. Corroborated in-tree: `branchsync
   --keep-local` deliberately stages objects "via gate-side fetch — never a push, which would fire the
   receive hook and start a run" (`AGENTS.md`). So a QA trigger must create runs **directly**.
5. **Connected gates must still install both hooks.** `pre-receive` is a *fail-closed admission guard*:
   anything that can reach `<NM_HOME>/repos/<id>.git` can push into it, and `gatecontext`
   peer-ancestry is what refuses. A connected gate is **more** exposed (no developer owns it; the
   objects belong to a third party). One gate shape also means **zero change** to `migrateGateConfigs`,
   `GateConfigCurrent`, and `MarkGateConfigCurrent`, which byte-compare the `pre-receive` script for
   every directory under `ReposDir()` on every daemon start.
6. **Gating `cli/root.go findRepo` is not sufficient.** `branchsync.OpenCurrent` does its **own**
   `GetRepoByPath` at `internal/branchsync/sync.go:163`, bypassing `findRepo`. From inside the stub it
   would return a live `Service` with `Apply`/`Recover` wired. Not self-closing.
7. **Read-only is enforced by `auto_fix.* = 0` plus never answering `fix`, not by the skip set.** Fix
   rounds mutate the worktree *during* review/test/lint and `commitAgentFixes` commits them and
   advances the run head, long before the push step.
8. **CORRECTED — `rebase` must NOT be skipped.** `rebase.go` contains **zero `git.Push*` calls**
   (`pushRemote` is used only for `isForcePushAgainstRemote` and fetch), and `tryRebase` runs
   `git rebase --abort` at four sites (`rebase.go:341,367,404,410`), leaving the worktree clean at the
   original head. So it is read-only against the remote, its history rewriting happens in the
   disposable worktree, and an unattended `approve` on a conflict gate safely yields the useful QA
   output "this branch does not rebase cleanly onto X". Skipping it would make every nightly report
   compare against a **stale base** and re-report already-fixed issues — contradicting `VISION.md:37`
   ("validation runs against the actual branch"). Its `SkipRemaining` on an empty diff
   (`rebase.go:525`) is also the largest available token saving for already-merged branches.
9. **CORRECTED — `intent` must NOT be skipped either.** `IsAuthoritativeRunIntentSource`
   (`run.go:511`) is a two-value whitelist, so a **new** source is automatically non-authoritative with
   zero edits. And the intent step already short-circuits when `run.Intent` is non-empty
   (`intent.go:55`). So supply a deterministic commit-derived intent at run creation. This is strictly
   better than skipping, and it closes a real defect: the intent step's readers scan **local** agent
   transcripts on the daemon host, which for a connected repo has no authoring session and could match
   the operator's *unrelated* work — attributing a stranger's intent to someone else's branch.
10. **`trigger` is safe as a column name.** I empirically ran CREATE/INSERT/SELECT with a bare
    `trigger` column against `modernc.org/sqlite`; all three succeed. (A claim that it is reserved and
    needs renaming is wrong — noted so it is not "fixed" later for a false reason.)
11. **Findings survive an unattended `approve`.** `db.CompleteStepWithStatus` (`db/step.go:182`) does
    **not** touch `findings_json`, and the executor persists findings *before* the gate decision
    (`executor.go:757`, `:785`). So report mode loses nothing by approving.
12. **Daemon-side `approvalCh` pushes are unsound.** `RespondWithOverrides` rejects when `!e.waiting`;
    worse, the loser of `claimGateReconciliation` does `return <-e.approvalCh`
    (`executor.go:1147-1173`), which **blocks forever** if a daemon push already landed and was drained
    by the deferred `select` at `executor.go:1126`. And `ParkStepForApproval` sets
    `awaiting_agent_since`, so an unattended run would briefly claim to await a human — the inverse of
    the lie `VISION.md:51` forbids. Resolve at the park *decision point* instead.
13. **There is no run concurrency control at all.** No global cap, no per-repo cap, no queue. Each
    trigger spawns a goroutine (`manager.go:960`) and an agent CLI subprocess; `db.Open` sets
    `SetMaxOpenConns(1)`. Per `repoID/branch` there is a mutex plus `cancelActiveRuns`, which
    **supersedes rather than queues**, waiting up to 30s. `branchLocks sync.Map` is never pruned.
14. **Active QA runs would block `daemon stop`.** `lifecycle.ActiveRuns` → `GetActiveRuns()` selects
    `status IN (pending, running)` (`run.go:205`), and `daemon stop|restart|update` refuse while any
    exist. A nightly sweep would make `daemon restart` need `--force` most nights, training operators
    to always pass it and defeating the guard for real work.
15. **The post-run hook runs before worktree removal.** `autoCaptureEvalCase` is the **last statement**
    of the run goroutine body (`manager.go:1044`), while `git.WorktreeRemove` is in the *deferred*
    block (~`manager.go:995`) — so a hook there still has the worktree, which the forge-comment path
    needs to construct a host.
16. **`scm.Host` cannot list PRs or post comments.** `CheckRerunner` (`scm/host.go:206`) is the
    established optional-capability pattern: a separate interface implemented only by GitHub, callers
    type-asserting so other backends keep compiling.
17. **URL normalization already exists.** `gate.inspectRefreshRemote`/`refreshRemoteIdentity`
    (`gate/refresh.go`) canonicalize to a credential-free `host/path`. One subtlety is load-bearing:
    for a userinfo-carrying `https://` remote they return a **non-nil error but still populate the
    identity**, so a promoted wrapper must key off `identity != ""` — operator URLs routinely carry a
    token.
18. **The trust boundary is already right for QA** and must be preserved: code-executing fields
    (`commands.*`, `agent`) plus `document.instructions`, `review.path_instructions`,
    `disable_project_settings`, `no_ci`, `ci.rerun_transient`, `test.evidence.branch` come only from a
    **pinned SHA on the trusted default branch**; `allow_repo_commands` defaults false.

---

## Design

### 1. Types

New `internal/types/runmode.go`: `RunKind{Gate,QA}`, `RunMode{FixPR,Comment,Report}`,
`NormalizeRunKind` (empty/unknown → `Gate`), `NormalizeRunMode` (empty → `FixPR`), and:

```go
// SkipSetFor returns the steps a run of this kind+mode declares it will not run.
// It returns nil for RunKindGate for ANY mode: a standing rule may never skip a
// step on an author's behalf (VISION.md L13), so the gate's nine steps are
// unreachable from configuration.
func SkipSetFor(kind RunKind, mode RunMode) []StepName {
    if kind != RunKindQA { return nil }
    switch mode {
    case RunModeFixPR:                    return nil
    case RunModeReport, RunModeComment:    return []StepName{StepDocument, StepPush, StepPR, StepCI}
    default:                              return nil // unknown degrades to the FULL pipeline, never a weaker one
    }
}
```

New `internal/types/gate.go` — `VISION.md:29` ("when two mechanisms would judge the same question,
they are reconciled into one, never stacked") forbids the daemon growing a second copy of
`gateResolution` (`cli/axi_drive.go:507`), so the rule moves to `internal/types` (zero deps, importable
by both `internal/cli` and `internal/pipeline`):

```go
// ResolveConvergingGate is the single owner of the rule that answers a gate so a
// run converges instead of looping: fix once with every finding selected when the
// gate carries actionable findings, otherwise approve.
func ResolveConvergingGate(f Findings, alreadyFixed, inFixReview bool) (ApprovalAction, []string)
```

`gateResolution` becomes a two-line adapter delegating to it. The existing `axi_drive_test.go` cases
passing unchanged is the real proof of behaviour preservation.

### 2. Skip sets, and why each step is in or out

| Mode | Skipped |
|---|---|
| `report` | `document`, `push`, `pr`, `ci` |
| `comment` | identical to `report` — the two differ **only** in publication |
| `fix-pr` | none |

- **`push`** — the only step that writes the branch to the target. Removing it *is* the read-only
  property and makes downstream writes structurally impossible.
- **`pr`** — it *creates or updates* a PR. A sweep must not open PRs on branches it merely observed.
  `comment` mode's forge write is a comment on a PR that **already exists**, posted by the post-run
  reporter.
- **`ci`** — a long-lived park whose idle timeout is `DefaultCITimeout = 7 * 24h`. An unattended sweep
  must never hold a worktree, agent subprocess, and branch lock for days — and there is nothing to
  babysit, since nothing was pushed.
- **`document`** — it *edits files*, and `commitAgentFixes` (`common_fix.go:118`) commits them,
  `update-ref`s the branch, and advances `Run.HeadSHA`. Those commits have nowhere to go.
  **Known trap:** when `Commands.Lint == ""` document also performs the lint duty and stashes it on
  `RunShared` (`document.go:104` → `lint.go:29` `TakeHousekeepingLint`). Skipping document means lint
  finds no stash and pays its own cold pass — correct and safe (the lint step's own comment says the
  duty "is never silently skipped"), and net agent cost is one pass either way. The real loss is that
  report mode yields **no documentation findings**; state that in the docs. The right follow-up is an
  `sctx.ReadOnly` flag suppressing `commitAgentFixes`, not un-skipping — out of scope here.
- **`rebase` — kept** (finding #8).
- **`intent` — kept** (finding #9), with a commit-derived intent supplied at run creation:

```go
// internal/db/run.go
// RunIntentSourceCommits marks an intent derived deterministically from the
// branch's own commit subjects rather than from an author's statement. It is NOT
// authoritative — IsAuthoritativeRunIntentSource deliberately does not list it —
// so prompts frame it as a low-confidence hint and a QA run can never assert
// acceptance criteria its subject never stated.
const RunIntentSourceCommits = "commits"
```

Text from `git.Log(ctx, gateDir, baseSHA, headSHA)`, truncated. Because the step short-circuits on a
non-empty `run.Intent`, **no change to the intent step is needed**, and no local transcript is ever
scanned for a connected repo.

Recorded twice, one derived from the other: `runs.skipped_steps` (the auditable declaration, written at
insert before any step ran) and `step_results.status='skipped'` (the mechanism, already written today
at `executor.go:184`). `ValidateRecoveredRun`/`recoveredGate` need **no change**: `e.steps` is always
nine, all nine rows are inserted up front, the positional check compares *names* not statuses
(`executor.go:442`), and the "prior steps completed **or skipped**" branch (`executor.go:478`) already
accepts skipped.

### 3. Unattended gate resolution

New `internal/pipeline/gatepolicy.go`:

```go
type GateRequest struct {
    Step types.StepName; Findings types.Findings; FindingsJSON string
    Fixing bool; AutoFixAttempts, Round int; Recovered bool
}
type GateDecision struct {
    Action types.ApprovalAction; FindingIDs []string; Reason string // bounded
}

// GatePolicy answers approval gates for a run with no human and no driving agent.
// A nil policy, or ok=false, leaves the gate parked exactly as today — so this is
// purely additive to the author gate, which never installs one.
//
// The executor consults a policy ONLY for steps that do not implement
// ApprovalGateReconciler. A reconcilable gate (today: CIStep) already has an
// external source of truth that resolves it without a human, so short-circuiting
// it would destroy the very mechanism that makes CI babysitting unattended.
type GatePolicy interface { ResolveGate(GateRequest) (GateDecision, bool) }

func (e *Executor) SetGatePolicy(p GatePolicy)
```

Two policies in `internal/qa/policy.go`:

- **`ReadOnlyGatePolicy`** — `approve`, always. Installed by `report` and `comment`.
  *Not `fix`*: a fix round runs an agent that edits files and commits them. *Not `skip`*: `skipped`
  erases the fact that the step ran and produced findings — every consumer reads it as "not checked".
  *Not `abort`*: that would fail the run at the first blocking finding, so a report would cover review
  and nothing after. Findings are not lost (finding #11); the verdict is derived from the persisted
  findings, never from the step status.
- **`ConvergingGatePolicy`** — delegates to `types.ResolveConvergingGate`. Installed by `fix-pr`. The
  fix-once-then-approve shape is the loop guard: without it, a finding no fix can clear cycles
  review → fix → fix_review → fix forever.

**Insertion point — exactly one place.** In `executeStep`, between the completion check
(`executor.go:832`) and the park freeze (`executor.go:842`):

```go
// Unattended resolution happens BEFORE any park side effect: before the execution
// timer freezes, before ParkStepForApproval writes awaiting_agent_since, before
// e.waiting is set, and before waitForApprovalOrReconcile is entered. A
// policy-answered gate is therefore never observable as parked, can never be
// answered over IPC, is never seen by recoverableParkedRuns, and can never race
// claimGateReconciliation.
if e.gatePolicy != nil {
    if _, reconcilable := step.(ApprovalGateReconciler); !reconcilable {
        if decision, ok := e.gatePolicy.ResolveGate(req); ok {
            e.db.SetStepRoundGateAction(currentRoundID, string(decision.Action), db.GateActionSourcePolicy, decision.Reason)
            writeLog(fmt.Sprintf("unattended gate: %s (%s)", decision.Action, decision.Reason))
            // falls into applyApprovalAction — the SAME code the human path runs
        }
    }
}
```

Refactor the existing `switch response.action` body (`executor.go:910-963`) into
`applyApprovalAction(...)` so the policy path and human path execute identical code. Two copies of that
switch is how skip/fix/abort semantics drift.

**Crash recovery.** `report`/`comment` runs never park, so `recoverableParkedRuns` never sees one; a
crashed one is marked `failed` and the next sweep re-derives it. A `fix-pr` QA run *can* be found
parked at the CI gate, so `recoveredRunPlan` gains `kind`, `mode`, `skips` read from the `runs` row, and
`resumeRecoveredRun` calls `SetSkippedSteps` + `SetGatePolicy` **before** `Resume`. `Resume`'s
post-reconcile fallback (`executor.go:329`) must apply the same rule — unreachable today, but right by
construction rather than by coincidence.

### 4. Concurrency

New `internal/daemon/runqueue.go`. `RunManager` gains `slots *runSlots`, acquired inside
`startRunWithIntentSource` **after** the branch lock and **before** `InsertRunWithIntent`, released in
the existing `defer`.

```go
// admitInteractive is a push, rerun, or crash-recovery resume. It is never
// rejected and never queued: "validation must not hold the author hostage"
// (VISION.md) is the product's core promise. Interactive runs COUNT toward the
// global total but are not GATED by it, so a burst of pushes transiently exceeds
// the ceiling and QA simply stops admitting until it drains.
// admitQA is scheduled or manual QA work: capped, queued, yields to interactive.
```

Because interactive `TryAcquire` never fails, **the push path's error surface and timing are
unchanged**. Defaults `max_concurrent=2`, `max_total_runs=4`: each run spawns agent CLI subprocesses
(routinely 1–3 GB RSS), every DB write serializes on `SetMaxOpenConns(1)`, and the daemon has already
been OOM-killed in the field by leaked grandchildren (`AGENTS.md`), so a *new* concurrency source
should be conservative. These are reasoned, **not measured** — instrument `qa status` and revisit.

- **Queue owned by `internal/watch`**, drained by exactly `max_concurrent` dispatchers, so at most that
  many goroutines are ever inside `startRunWithIntentSource` and total time in `cancelActiveRuns`' 30s
  wait is bounded by `max_concurrent × 30s` rather than by the number of watched refs.
- **Nothing is silently dropped.** *Scheduled* items coalesce on `(repoID, branch, mode)`; at
  `queue_depth` the **oldest scheduled** item is evicted with an INFO log naming what and why —
  lossless, because the next sweep re-derives it from `watch_state`. *Manual* items are never evicted:
  `qa_run` returns a real error to the CLI. *Pushes* never touch this queue.
- **Supersede rules:** a push/rerun supersedes everything on the branch including QA runs (correct
  as-is, no change — the author's push is newer truth). **A QA run never supersedes an active gate
  run**: under the branch lock, before `cancelActiveRuns`, check `GetActiveRunsForBranch` and decline
  if any is `run_kind='gate'`. A QA run may supersede an older QA run (existing semantics).
- **Replace `branchLocks sync.Map` with a refcounted `branchLockSet`** that deletes an entry when its
  last holder releases. Bounded by human pushes today; a sweep over many repos × refs makes it grow
  without ceiling. Correctness rests on incrementing the refcount under the set mutex **before** taking
  the per-key mutex, so a concurrent release cannot delete an entry a waiter is about to block on.
- **`lifecycle.BlockingRuns`** (finding #14): `ActiveRuns` keeps returning everything (the honest
  answer), and a new classifier decides what a destructive lifecycle op must actually protect. A
  report/comment QA run never pushed and never opened a PR, so cancelling it costs tokens, not code,
  and the next sweep re-derives it. A `fix-pr` QA run **does** block — "the tool must not lose people's
  code" draws no distinction between a gate run's commits and a QA run's. `CancellableRuns` is the
  complement so the guard can report "also cancelling 3 QA runs" rather than silently discarding them.
- **`qa.max_run_duration`** (default 6h): the watcher, which owns the run's identity, cancels via the
  existing `HandleCancel` with a new `RunCancelReasonQATimeout`, so the run terminalizes as `cancelled`
  through the existing `failRun` path and custody/`branchsync` semantics are unchanged.

### 5. Schema — additive only

Append to `migrationStatements`, **in this order**:

```go
`ALTER TABLE repos ADD COLUMN source_kind TEXT NOT NULL DEFAULT 'local'`,
`ALTER TABLE repos ADD COLUMN source_identity TEXT`,
`ALTER TABLE repos ADD COLUMN source_name TEXT`,
`ALTER TABLE repos ADD COLUMN detached_at INTEGER`,
`ALTER TABLE runs ADD COLUMN run_kind TEXT`,     // NULL reads back as the author gate every old row was
`ALTER TABLE runs ADD COLUMN run_mode TEXT`,
`ALTER TABLE runs ADD COLUMN trigger TEXT`,      // verified safe as a column name (finding #10)
`ALTER TABLE runs ADD COLUMN skipped_steps TEXT`,
`ALTER TABLE runs ADD COLUMN watch_id TEXT`,
`ALTER TABLE runs ADD COLUMN qa_verdict TEXT`,   // about the CODE; runs.status stays about the RUN
`ALTER TABLE runs ADD COLUMN qa_report_error TEXT`,
`ALTER TABLE step_rounds ADD COLUMN gate_action TEXT`,
`ALTER TABLE step_rounds ADD COLUMN gate_action_source TEXT`, // human|policy|reconciler|auto-fix
`ALTER TABLE step_rounds ADD COLUMN gate_action_reason TEXT`,
`CREATE UNIQUE INDEX IF NOT EXISTS idx_repos_source_identity ON repos (source_identity)`,
```

Two traps: SQLite allows `ADD COLUMN ... NOT NULL DEFAULT <constant>`, so every existing row reads
`'local'` with no rewrite. And **the unique index must go last in `migrationStatements`, never in
`schemaSQL`** — `CREATE TABLE IF NOT EXISTS` is a no-op on an existing DB, so in `schemaSQL` the index
would run before the column existed and `Open()` would hard-fail (it tolerates only duplicate-column
errors). SQLite unique indexes ignore NULLs, so existing local rows coexist.

Two new tables in `schemaSQL` (whole-table additions belong there; `migrationStatements` is for
columns only): `repo_watches` (`id`, `repo_id` FK CASCADE, `kind` branch|pr, `pattern`, `mode`,
`enabled`, `poll_interval_seconds`, `daily_at`, timestamps, `UNIQUE(repo_id,kind,pattern,mode)`) and
`watch_state` (`watch_id` FK CASCADE, `ref`, `last_seen_sha`, `last_run_id`, `last_status`,
`last_run_at`, `consecutive_failures`, `updated_at`, PK `(watch_id, ref)`).

`internal/db/repo.go` gains the four fields, `SourceKindLocal`/`SourceKindConnected`,
`func (r *Repo) Connected() bool`, and the columns in every SELECT list (`GetRepos`, `GetRepo`,
`GetRepoByPath`, `ReplaceRepoURLs`, `stats.go:258`) wrapped in `COALESCE`. **Do not widen
`InsertRepoWithIDAndFork`** — the local path stays byte-identical.

### 6. Config — operator-owned, global-only, credential-safe

Follow the **`Eval` precedent exactly**: present on `globalConfigRaw` + `GlobalConfig`, **absent from
`RepoConfig`**. `parseRepoConfig` uses plain `yaml.Unmarshal`, so a `qa:`/`repos:` block in a watched
repo's `.no-mistakes.yaml` loads and is **silently ignored**; `LoadGlobalFromBytes` uses
`KnownFields(true)`, so only `config.yaml` can declare it. In `Merge`, add `QA: global.QA` beside
`Eval: global.Eval` (`config.go:1775`) with the same "global-only, no repository override step" comment.

```yaml
qa:
  enabled: false            # default off
  poll_interval: 15m
  nightly_at: "02:30"       # optional daily anchor
  max_concurrent: 2
  max_total_runs: 4
  queue_depth: 64
  max_run_duration: 6h
  max_reports: 500
  default_mode: report
  comment: false            # global switch; a forge write needs this AND mode: comment
repos:
  acme-api:
    url: git@github.com:acme/api.git
    credential_env: ACME_TOKEN        # env var read at provisioning time only
    default_branch: main
    commit_name: "QA Bot"
    commit_email: "qa@example.com"
    disable_project_settings: true    # operator floor; can only ADD restriction
```

**Credential handling (finding #1) — both mitigations required:**
1. **`credential_env`** names an env var in the daemon's environment, injected into the URL only when
   the gate remote is written, so the token need never sit in `config.yaml`. Credential-free setups
   (ssh keys, credential helper, `gh auth`) need nothing — `git.NonInteractiveEnvFrom` already sets
   `GIT_TERMINAL_PROMPT=0` so an unauthenticated fetch fails fast. Add a `doctor` check.
2. **Redact `SourceYAML` anyway**, because operators paste tokens regardless: have
   `LoadGlobalFromBytes` rewrite `repos.*.url`/`fork_url` through `safeurl.Redact` before assigning
   `cfg.SourceYAML`. Redaction is idempotent and `SourceYAML` is only used for replay provenance, where
   a redacted URL suffices. Also `slog.Warn` when `config.yaml` is group/other-readable and any URL
   carries userinfo.

Validation at parse time beside `validateEvalRaw`: name `^[a-z0-9][a-z0-9._-]{0,63}$` and never
`.`/`..` (it must not reach a path); `url` must yield an identity; two names resolving to one identity
is an error; valid mode; valid `daily_at`; `max_concurrent > 0`. **`mode: fix-pr` is rejected for any
watch** — unattended runs never write code; it stays available to `qa run --mode fix-pr --yes`.
**`fork_url` is rejected for connected repos** in this scope: `validateForkRouting` is GitHub-parent +
GitHub-fork only.

**Scheduling uses the standard library only** — `poll_interval` + `nightly_at` covers both requested
triggers. Cron would need a third-party library, which per `AGENTS.md` requires discussing the
dependency with you first; the `Schedule` type is the seam that follow-up would sit behind.

### 7. Connected-repo registration

New `internal/gate/connected.go`. Promote the existing normalizer rather than writing one:

```go
// RemoteIdentity returns the canonical credential-free identity "<host>/<path>".
// Keys off identity != "" because inspectRefreshRemote reports an error for a
// userinfo-carrying https remote while still populating the identity, and an
// operator URL routinely carries a token (finding #17).
func RemoteIdentity(raw string) (string, error)

// ConnectedRepoID: hex(sha256("no-mistakes/connected/v1\x00"+identity)[:6]).
// The domain-separation prefix keeps the two ID namespaces from being each
// other's preimages. Same 12-lowercase-hex shape, so RepoDir(id) -> <id>.git
// stays valid for repoIDFromGatePath, migrateGateConfigs' legacy scan, and
// gatecontext's TrimSuffix(base, ".git"). NEVER derive the ID from the
// operator-chosen name — a name could escape RepoDir.
func ConnectedRepoID(identity string) string

// ProvisionConnectedGate is provisionGate minus ensureWorkingRemote (there is no
// developer clone). Everything else — bare init, receive.advertisePushOptions,
// RefreshManagedGateHooks (BOTH hooks, finding #5), hooks-path isolation +
// MarkGateConfigCurrent, credentialled origin — is identical, so
// migrateGateConfigs, GateConfigCurrent, and gatecontext see one gate shape.
func ProvisionConnectedGate(ctx context.Context, bareDir, upstreamURL string) error

func EnsureConnected(...)  ; func ReconcileConnected(...) ; func RemoveConnected(...)
```

Refactor `provisionGate` (`gate.go:185`) into `provisionGateCore` plus the `ensureWorkingRemote` tail so
hook logic is never duplicated.

`EnsureConnected` order: resolve identity → collision check → provision gate → `ensureIdentityStub`
(finding #3: resolve identity from config → daemon global git config → **error**) → insert/update →
clear `detached_at` → default branch via `git.DefaultBranch(ctx, p.RepoDir(id), "origin")` (its
`ls-remote --symref` works from the bare gate through `git.RunBare`). On failure before insert, remove
only what this call created — mirroring `InitWithFork`'s `existing == nil` discipline, never tearing
down an already-registered gate on a repair failure.

**Collisions:** the unique index, plus a pre-insert refusal when `GetRepo(id)` exists with a different
identity or is `local` (a 48-bit collision with a path hash), requiring an explicit `id_salt` to
disambiguate. Never silently reuse or overwrite.

New `paths` methods (+ `SourcesDir()` in `EnsureDirs`; **`QAReportsDir` deliberately not** created
there, per the `EvalDir` rule that disabling a feature creates no state):

```go
func (p *Paths) SourcesDir() string
// SourceDir is a connected repo's identity stub. Deliberately NOT a clone: the
// objects live in the gate. It exists so a connected repo has a unique
// daemon-owned working_path satisfying NOT NULL UNIQUE without a table rebuild,
// and a place to pin the commit identity CopyLocalUserIdentity copies per run.
func (p *Paths) SourceDir(repoID string) string
// QAReportsDir holds durable QA reports. Unlike test evidence — whose root is
// <os.TempDir()>/no-mistakes-evidence, acceptable there because the PR body is
// the durable record — a QA report is the ONLY record an unattended sweep
// produces, so it lives under NM_HOME.
func (p *Paths) QAReportsDir() string ; func (p *Paths) QARunReportDir(runID string) string
```

**URL refresh and invariant C1.** Add at the top of `RefreshRepoURLs`:
`if repo.Connected() { return refreshFailure(RefreshConfigMismatch) }`, and a separate
`RefreshConnectedRepoURLs(ctx, d, p, repo, ConnectedTarget{URL, ForkURL})`.

> **C1:** `safeurl.Redact(<gate origin>) == repos.upstream_url` for every connected repo.

`resolveUpstreamURL` (`common_git.go:163`) returns the worktree origin only when
`!URLsVerified || Redact(origin) == repo.UpstreamURL`, so C1 is what keeps credential recovery working.
**Only one write order is safe:** redacted DB row **first**, then the gate remote; on remote-write
failure roll the row back and report unreconciled. The reverse leaves a window where a fallback
resolves the **old** URL and a push could target the **previous repository**. Add
`AssertConnectedGateURLBinding`, called from the connected branch of `startRunWithIntentSource` after
the refresh and before `git.WorktreeAdd`, with a URL-free error.

**Deliberate asymmetry:** the local path logs "refresh skipped; continuing" and continues
(`manager.go:762`); the connected branch must **fail the run**. For a local repo the fallback is the
operator's own row about their own clone; for a connected repo a stale row may name a *different
repository*.

**Reconciliation is strictly additive.** For each spec, `EnsureConnected`; for each connected row whose
identity is absent from the spec set, `SetRepoDetachedAt` and log — **never** `DeleteRepo`, never
`RemoveAll` a gate or worktrees. Re-adding the URL clears `detached_at` and reuses the same
identity-derived ID, so the gate and full run history survive — the concrete payoff of identity-derived
IDs. `source_kind='local'` rows are never read for detach and never written. Skip an entry whose URL
changed while an active run exists. Deletion is only ever
`no-mistakes repos remove <name> [--delete-history]`, which refuses while active runs exist (reusing
`lifecycle.ActiveRuns`/`RunList`). Runs from `recoverOnStartup` **before** `migrateGateConfigs`
(`daemon.go:368`).

### 8. Making author-side surfaces inert for connected repos

The choke point in `cli/root.go findRepo` (refuse when `repo.Connected()`) covers `init`, `eject`,
`attach`, `rerun`, `status`, `sync`, `runs`, `stats`, the wizard, and every `axi` surface. Then the
sites that **bypass** it:

1. `branchsync.OpenCurrent` (`sync.go:163`) — **the verified hole (finding #6)**; refuse. This also
   disarms the TUI with no TUI change, since `tui/app.go:493` is
   `if service, closeFn, err := branchsync.OpenCurrent(); err == nil`.
2. `branchsync.inspect` (`sync.go:912`) — new `StateNotApplicable`, no `NextAction`; `CanApply` false.
3. `branchsync.Apply` / `Recover` — assert `!Connected()` at entry rather than trusting derived state;
   these are the only worktree-mutating entry points and this repo's convention is self-guarding.
4. `cli/sync.go openSyncService`, `cli/status.go:65`, `cli/axi_drive.go` (`inspectAxiBranchSync`,
   `freshRunBranchOwnershipState`) — return not-applicable so `recover_custody`/`user_owned` is never
   offered for a repo with no local branch.
5. `steps/rebase.go detectBundledLocalDefaultCommits` (line 185) — return nil early. It reads
   `refs/heads/<default>` from `Repo.WorkingPath`; benign on a refless stub, but it is a **blocking,
   non-auto-fixable** finding and must not depend on the stub staying refless.
6. `gate.Eject` — refuse, point at `repos remove`. `gate.InitWithFork` — refuse roots under
   `SourcesDir()`, `ReposDir()`, `WorktreesDir()` (the latter two close a pre-existing gap).
7. `db/stats.go:42` — display `source_name`, not `filepath.Base(WorkingPath)` (which prints the hex ID).

**Do not gate** `steps/push.go:121` or `steps/ci_fix.go:186` (`branchsync.TargetFingerprint`) — a
credential-free fingerprint of the push target that must keep working. `gatecontext`,
`migrateGateConfigs`, and `GateConfigCurrent` need **no change**: the stub's common dir is
`<NM_HOME>/sources/<id>/.git`, whose parent is not `ReposDir()`, and there is one gate shape.

### 9. Operator-side project-instruction floor

**effective = trustedRepoValue OR operatorFloor**, the floor a plain `bool` so absent/false is
structurally a no-op. No YAML value can clear a repo's own trusted `true`.

```go
type RepoPolicy struct{ DisableProjectSettings bool }
func (g *GlobalConfig) RepoPolicyFor(name string) RepoPolicy
// MergeWithRepoPolicy is Merge plus the operator's monotone floor. Merge is
// preserved unchanged and equals MergeWithRepoPolicy(g, r, RepoPolicy{}).
func MergeWithRepoPolicy(global *GlobalConfig, repo *RepoConfig, p RepoPolicy) *Config
```

Two call sites change, both in `internal/daemon/manager.go`: `startRunWithIntentSource` (line 878) and
`loadRecoveredConfig` (line 235), passing `RepoPolicyFor(repoPolicyKey(repo))` which returns
`repo.SourceName` for connected repos and `""` for local (so local behaviour is unchanged).
`newPipelineAgent` needs **no change** — it already reads `cfg.DisableProjectSettings` for
`agent.Options` and the fail-closed `EnsureGateNeutralized`. Applying the floor in the merge rather than
at the agent site is what makes it reach both fresh and recovered runs. `EffectiveRepoConfig` is **not**
touched: `disable_project_settings` stays trusted-only there, and the floor is a separate later lift.

**Default the floor `true` for connected repos** — the operator cannot edit a watched repo, so its
`AGENTS.md`/`CLAUDE.md` is untrusted input to the gate agent. Consequence to accept: with the floor on
and `EnsureGateNeutralized` failing closed, only `codex`, `claude`, and `pi` have a verified suppression
knob, so an operator whose only agent is `opencode`/`rovodev`/`copilot` needs an explicit,
loudly-logged per-repo `allow_project_instructions: true` that lowers **only the connected default**,
never a repo's own trusted `true`.

### 10. Watcher — `internal/watch`

Named for the product's own vocabulary. It does **not** import `internal/pipeline` or
`internal/daemon`; it drives runs through injected interfaces, mirroring the existing `StepFactory`
seam (`daemon/manager.go:31`).

```go
type Starter interface {
    StartQARun(ctx context.Context, req QARunRequest) (runID string, err error)
    AwaitRun(ctx context.Context, runID string) (types.RunStatus, error)
    CancelRun(runID string) error
}
// Fetcher refreshes a connected repo's bare gate from its remote.
type Fetcher interface { FetchGate(ctx context.Context, repo *db.Repo) error }
```

Files: `watch.go` (Watcher, tick loop, dispatcher pool, queue), `discover.go`, `refmatch.go`,
`schedule.go`.

**Lifecycle** in `daemon.runWithOptionsLocked`: constructed after `NewRunManager`, `Start`ed **after**
`confirmLocalIPCHealth` (so a slow first sweep cannot eat the 45s startup budget), `Stop`ped in
`doShutdown` **before** `mgr.Shutdown()` so no new QA run is admitted while runs are being cancelled.
Skipped entirely when `!cfg.QA.Enabled`.

**Schedule** — a single `time.Timer` reset to `NextFire(...)` each iteration, not a `Ticker`, so
interval and daily anchor share one wakeup and clock jumps are absorbed. `NextFire` is recomputed from
a fresh `time.Now()` rather than accumulated, so a suspended laptop, a DST shift, or an NTP step cannot
desynchronize it permanently. `MissedAnchor` makes a laptop closed at 02:30 run its nightly sweep once
at wake instead of skipping the night silently.

**Discovery.** Fetch the gate first (connected repos only), then enumerate refs locally — a fetch of
`refs/heads/*` is one network op, so no separate remote probe is needed. One new helper:

```go
// ListBranchRefs returns refs/heads/* -> object name. It uses RunBare so a
// malformed or non-bare directory cannot discover an ancestor worktree — the same
// fail-closed reason startup gate migration uses RunBare.
func ListBranchRefs(ctx context.Context, bareDir string) (map[string]string, error)
```
(`for-each-ref --format=%(refname)%09%(objectname) refs/heads/`.) Filter with `MatchAny`; reject
deletions via the existing `git.IsZeroSHA`; compare against `watch_state.last_seen_sha`; resolve the
base with `merge-base` in the bare gate; derive the commit intent with `git.Log`.

**Poll-triggered watches apply only to connected repos** — a local repo's gate is written exclusively
by the developer's push, so polling it can never see anything a push has not already triggered.
Nightly and manual triggers work for both kinds.

**`watch_state` advancement is the crash-safety property.** `last_seen_sha` advances **only after** the
run reaches a terminal status **and** only if the SHA observed at dispatch still matches. A crash
mid-run leaves it at the previous value and the next sweep re-derives the work — which is what makes it
safe to simply fail crashed QA runs. On failure, `consecutive_failures` increments and the next fire for
that ref is delayed by `min(interval × 2^failures, 24h)`, so a repo whose agent is broken does not burn
tokens every 15 minutes.

**Ref matching — new `internal/watch/refmatch.go`.** It must **not** reuse `matchIgnorePattern`
(`steps/common_diff.go:87`), whose PATH semantics make a slash-free pattern match the **basename**:
applied to refs that is silently dangerous — `"main"` would match `refs/heads/feature/main` and a watch
intended for one branch would sweep every branch named `main` in any namespace. Refs are hierarchical
names, not paths, so there is no basename fallback. Semantics: `main` → exactly that; `release/*` →
`release/1.0` but not `release/1.0/hotfix` (`path.Match` already refuses to let `*` cross `/`, so only
the `/**` suffix and bare `**` need explicit branches); `**` → every branch. `ValidatePattern` rejects a
typo at `qa watch add` time rather than silently matching nothing every night.

**PR watching.** Phase 6 uses the **existing** `Host.FindPR(ctx, branch, base)` (`scm/host.go:184`),
which every backend implements: for each matching branch ref in the gate it answers "is there an open
PR", and update detection reuses the branch SHA already in hand. Stated limitation: a PR from a *fork*
whose head branch is absent from the upstream repo is invisible this way. Phase 8 adds optional
`scm.PRLister` (`ListOpenPRs`) on the `CheckRerunner` precedent — GitHub only, via
`gh pr list --json …` with the existing `repoArgs()`; other providers log a bounded "PR watching is not
supported on <provider>" **once per watch per daemon lifetime**, not every sweep.

### 11. Reporting — post-run hook, not a step

A tenth step would need a slot in the order hardcoded in **two** places (`types.StepName`/`Order()`/
`AllSteps()` and `steps.AllSteps()`), would break `Order()`'s 1..9 contract, and would contradict
"reuse with skips". `autoCaptureEvalCase` (`manager.go:1070`) is the established shape and all five of
its properties are copied: recover its own panic (the run goroutine's enclosing recover would otherwise
mark a **finished** run failed), bound its own time, serialize on a mutex (it prunes one shared
directory), and report failure only to the log and `runs.qa_report_error`.

New `internal/qareport`: `Verdict{Clean,Findings,Blocked}`, a versioned `Report` struct, and
`Build(database, runID)` deriving everything from **persisted step results and rounds only** — never
the worktree — so the report stays valid after cleanup and is reproducible from the DB alone.
`ErrNoQAReport` separates "nothing to do" (DEBUG) from a real fault (WARN), exactly as
`eval.ErrNoCapturableReview` does. Verdict derivation: any step `failed` or a rebase conflict finding →
`blocked`; else any blocking or effective-`ask-user` finding → `findings`; else `clean`.

Called as the **last statement** of the run goroutine body, right after `autoCaptureEvalCase`
(`manager.go:1044`) — after the outcome is decided so nothing here can change it, but **before** the
deferred cleanup, so the worktree still exists for host construction (finding #15). Artifacts:
`<QARunReportDir>/report.json` + `report.md`, path in `runs.qa_report_path`, retention `qa.max_reports`
pruned oldest-first, **never** pruning a report whose run is still active.

Reuses `steps.BuildPipelineSummary` and `steps.BuildTestingSummary`, both already exported and DB-only.
**Caveat flagged, not pre-solved:** importing `internal/pipeline/steps` pulls in agents and every SCM
backend. Acceptable initially; if it bites, the fix is a mechanical move of those two functions into a
leaf `internal/prsummary` used by both — a move, not a behaviour change. Host construction needs one
small export, `steps.BuildHostForRepo`, delegating to `buildHost` so provider selection and fork-routing
refusals keep exactly one owner.

**Forge comment** — new optional `scm.ReviewCommenter` on the `CheckRerunner` precedent:

```go
type ReviewCommenter interface {
    // FindComment matches on body CONTENT, never author or position, so it holds
    // regardless of which token posted the original — the same content-not-name
    // discipline that makes evidence.MarkerPath safe.
    FindComment(ctx context.Context, pr *PR, marker string) (string, error)
    UpsertComment(ctx context.Context, pr *PR, commentID, body string) (string, error)
}
```

Marker is keyed on `(watchID, headSHA)`: a sweep over an unchanged head produces the **same** marker so
the reporter **edits its own previous comment instead of adding another**; a new head produces a new
marker so per-commit history is preserved. Keep the encoding byte-stable — a changed marker orphans
every previous comment and the next sweep posts duplicates.

**Fail closed on every ambiguity, like `assertEvidenceBranch`:** no `qa.comment: true` → never post
(two independent opt-ins required); no PR → no comment; host lacks the interface → bounded log;
**`FindComment` errored → post nothing** (a read failure is *not* evidence that no comment exists, and
"must not spam a PR" is the stronger requirement — this is the single most important fail-closed
decision here); `UpsertComment` failed → WARN + `runs.qa_report_error`, run outcome untouched.

**A failed comment must not fail the run:** the outcome is already decided, the findings are durable in
`step_results`/`step_rounds` and the local report, and a forge outage would otherwise turn every nightly
sweep into a wall of failed runs masking the real findings.

### 12. Run status semantics

**A `report`-mode run with blocking findings is `completed`.** `runs.status` answers "did the pipeline
execute to its end", not "is the code good" — already true today, since a gate run whose review is
approved with blocking findings ends `completed`. Every consumer treats `failed` as an **operational**
signal: `lifecycle.ActiveRuns` gates destructive lifecycle; `branchsync`'s `terminalRunStatus`
(`sync.go:1314`) treats terminal statuses alike for custody; the `axi` renderers and TUI colour
`failed` as "the run broke". Overloading it would make a repo with 30 findings look like 30 broken runs
and have `daemon stop` and custody recovery reasoning about code quality — a direct violation of "every
judgment has exactly one owner" (`VISION.md:29`).

The verdict rides on the nullable `runs.qa_verdict` plus `ipc.RunInfo.QAVerdict *string` with
`omitempty`. Every existing consumer is untouched **by construction**, because it reads `Status`, and
`qa_verdict` is NULL for every gate run and every historical row. `blocked` is how "could not validate"
is expressed honestly: the run completed, and the verdict says the validation did not.

### 13. IPC and CLI

New `ipc` methods, registered in `registerHandlers` (`daemon.go:618`): `qa_watch_add`,
`qa_watch_remove`, `qa_watch_list`, `qa_run`, `qa_sweep`, `qa_status`, `qa_report`. Every **mutating**
method goes through `refuseNested(ctx, false)` like `MethodRerun`, so a gate agent inside a pipeline
cannot start QA work recursively (`internal/gatecontext` stays the single classifier). The read methods
log at DEBUG — the existing `TestSuccessfulReadRequestsDoNotLogAtInfo` will catch a mistake for free.

```
no-mistakes repos list | reconcile | show <name> | remove <name> [--delete-history] [--yes]
no-mistakes qa watch add --repo <name> --branches 'main,release/*' --mode report [--interval 15m] [--nightly-at 02:30]
no-mistakes qa watch add --repo <name> --prs --mode comment
no-mistakes qa watch list | rm <watch-id>
no-mistakes qa run --repo <name> [--branch <b>] [--mode report|comment|fix-pr] [--yes]
no-mistakes qa sweep [--repo <name>]
no-mistakes qa status
no-mistakes qa report <run-id> [--json]
```

`--mode fix-pr` prints an explicit consent line naming the push target and the PR it may open and
**requires `--yes`** (`VISION.md:31`: "always explicit consent for a bounded scope, never a quiet
default"). Repo selection: new `resolveRepo(d, selector)` in `cli/root.go` — empty keeps today's cwd
behaviour, non-empty resolves by name, then full ID, then unique ID prefix; add `--repo` to `status`,
`runs`, `rerun`, `axi status`, `axi logs`, `axi abort`.

Docs, honouring the one-owner map in `AGENTS.md`: `reference/global-config.md` gains the `qa`/`repos`
keys; `reference/cli.md` the new commands; a new `concepts/qa.md` owns the run-kind/mode/skip-set model
and the verdict-vs-status distinction; `concepts/pipeline.md` gains one paragraph pointing there and
clarifying that "always all nine" means nine recorded step results; `concepts/daemon.md` (owner of the
lifecycle model) gains the watcher's start/stop position and the `BlockingRuns` classification.

---

## Specs

23 specs, each **one PR**: small enough to review in a sitting, independently testable, and leaving the
tree green. Stages A–C ship **no new capability** — the gate path is untouched and connected repos stay
unreachable — so every risky surface is already inert before anything can create one. **S18 is the first
genuinely useful milestone** and a legitimate place to stop.

`↳` marks the specs that must land in order; the rest of a stage can be reordered freely.

### Stage A — inert groundwork

**S1 ✅ Inert foundations** — `cac0974`. Types (`runmode.go`, `gate.go` + `gateResolution` delegation),
schema columns + two tables, `db/watch.go`, `paths` methods, `gate.RemoteIdentity`, `ConnectedRepoID`,
`provisionGate` → `provisionGateCore`, `internal/watch/refmatch.go`, `git.ListBranchRefs`.
*Done when:* every existing test passes unchanged.

**S2 ✅ `SourceYAML` credential redaction** — `65ac9e5`. Finding #1. Landed alone because it is the one
ship-blocking item and must precede any path that can put a URL in config.
*Tests:* `TestGlobalSourceYAMLRedactsConnectedRepoCredentials`,
`TestStoredStepRoundGlobalConfigNeverContainsUserinfo`.

**S3 — Author surfaces refuse connected repos** · *depends: S1*
All seven sites in §8: `findRepo`, `branchsync.OpenCurrent`/`inspect`/`Apply`/`Recover`, `cli/sync.go`,
`cli/status.go`, `cli/axi_drive.go`, `rebase.detectBundledLocalDefaultCommits`, `gate.Eject`/`InitWithFork`,
`db/stats.go` display. Guards only — no new capability.
*Done when:* a **hand-inserted** connected row makes `sync`, `status`, every `axi` surface, the TUI, and
`eject` refuse, invoked from inside the stub dir. Landing this before registration exists is the point:
no later bug can reach a live branchsync path.
*Tests:* `TestBranchSyncOpenCurrentRefusesConnectedRepoFromStubCWD` (finding #6 — the verified hole).

**S4 — `repos:` config block** · *depends: S1*
Parse + validate only, no consumer. Name charset, `.`/`..` refusal, identity resolution, duplicate-identity
error, `credential_env`, `fork_url` rejection for connected repos. Global-only, `Eval` precedent.
*Tests:* `TestRepoConfigCannotDeclareConnectedRepos`, validation table.

**S5 — `qa:` config block** · *depends: S1*
Same shape: parse + validate, no consumer. Modes, `daily_at`, `max_concurrent > 0`, **`mode: fix-pr`
rejected for any watch**.
*Tests:* `TestRepoConfigCannotChangeQAPolicy`.

**S6 — Operator project-instruction floor** · *depends: S4*
`RepoPolicy`, `MergeWithRepoPolicy`, the two `manager.go` call sites, the `allow_project_instructions`
escape hatch. `Merge` preserved as `MergeWithRepoPolicy(g, r, RepoPolicy{})`.
*Tests:* `TestOperatorPolicyCannotClearTrustedDisableProjectSettings`.

**S7 — `VISION.md` amendment + `concepts/qa.md`** · *no code*
The two-scope statement, and the doc that owns the run-kind/mode/skip-set model and verdict-vs-status.
Land it **before Stage B**: it is the product-level authorization for the whole second mode, and writing
it may still change the design.

### Stage B — registration

**S8 — Connected gate provisioning** · *depends: S3, S4* `↳`
`internal/gate/connected.go`: `ProvisionConnectedGate`, `ensureIdentityStub`, `EnsureConnected`, collision
refusal. No CLI, no reconcile, no refresh guard.
*Done when:* a connected repo can be registered against a local bare "remote" in a temp dir.
*Tests:* `TestEnsureIdentityStubFailsWhenNoCommitIdentityResolvable` (finding #3),
`TestConnectedRepoIDIsStableAcrossTransports`, `TestRemoteIdentityAcceptsCredentialBearingHTTPSRemote`.

**S9 — URL refresh guard + C1 binding** · *depends: S8* `↳`
`RefreshRepoURLs` refuses connected; `RefreshConnectedRepoURLs` with the **redacted-row-first** write
order; `AssertConnectedGateURLBinding`. Security-critical, hence its own spec.
*Tests:* `TestRefreshRepoURLsRefusesConnectedRepos`,
`TestRefreshConnectedRepoURLsWritesRedactedRowBeforeGateRemote`.

**S10 — Reconciliation + `repos` CLI** · *depends: S9* `↳`
`ReconcileConnected` (additive-only; detach, **never** delete), `RemoveConnected`, startup wiring before
`migrateGateConfigs`, `repos list|reconcile|show|remove`.
*Tests:* `TestReconcileConnectedNeverDeletesAnyRepoOrHistory`,
`TestReconcileConnectedNeverTouchesLocalRepos`, `TestDetachedConnectedRepoReattachesWithSameIDAndHistory`.

**S11 — Run-start support for connected repos** · *depends: S10* `↳`
The connected branch in `startRunWithIntentSource`: refresh, assert binding, **fail the run** on refresh
failure (the deliberate asymmetry), carve the worktree from the connected gate.
*Done when:* a normal gate-kind run can be driven against a connected repo by direct manager call.

### Stage C — concurrency (hard prerequisite for Stage E)

**S12 — Run slots + branch lock set** · *depends: S1*
`runSlots` (interactive never gated), refcounted `branchLockSet`, QA-never-supersedes-an-active-gate-run.
*Tests:* `TestQARunConcurrencyCapNeverDelaysAPush`, `TestBranchLocksDoNotGrowUnbounded`,
`TestQARunDoesNotSupersedeAnActiveGateRun`, `TestBranchLockStillSerializesConcurrentStartsForSameBranch`.

**S13 — Lifecycle classification** · *depends: S12*
`lifecycle.BlockingRuns`/`CancellableRuns`, `qa.max_run_duration` + `RunCancelReasonQATimeout`.
*Tests:* `TestDaemonStopIsNotBlockedByReportModeQARuns`, `TestDaemonStopIsBlockedByFixPRQARun`.

### Stage D — one QA run, on demand

**S14 ✅ Extract `applyApprovalAction`** · *pure refactor, no behaviour change*
Lifted the `switch response.action` body (was `executor.go:910-963`) into
`internal/pipeline/approval_action.go`. Shipped alone precisely so the diff that later adds the policy
path contains **no** restructuring. Every existing executor/approval test passes untouched; the seam's own
contract (one case per action, plus the preserved unrecognized-action retry) is pinned by
`executor_approval_action_test.go`.

**S15 ✅ `GatePolicy` seam + both policies** · *depends: S14* `↳`
`internal/pipeline/gatepolicy.go`, the two policies in `internal/qa/policy.go`, the single insertion point
before any park side effect, and the recovery reinstall. Tests landed as
`internal/pipeline/gatepolicy_test.go`, `internal/qa/policy_test.go`,
`internal/daemon/gatepolicy_test.go`, and the `db` gate-action round-trip tests.
*Tests:* `TestGatePolicyNilPreservesBlockingGate`, `TestGatePolicyIsNeverInstalledForGateRuns`,
`TestPolicyIsNotConsultedForReconcilableCIGate`, `TestFixPRPolicyFixesOnceThenApproves`,
`TestReportModeRunNeverSetsAwaitingAgentSince`, `TestPolicyResolvedGateRejectsIPCRespond`,
`TestRecoveredQARunReinstallsItsGatePolicyAndSkipSet`.

**S16 — Skip sets + commit-derived intent** · *depends: S15* `↳`
Apply `SkipSetFor` at run start, record `runs.skipped_steps`, supply `RunIntentSourceCommits`.
*Tests:* `TestSkipSetForIsExhaustiveOverModes`, `TestReportAndCommentSkipSetsAreIdentical`,
`TestValidateRecoveredRunAcceptsQARunWithSkippedSteps`, `TestRunSkippedStepsColumnMatchesSkippedStepResults`,
`TestSkippedDocumentStepFallsBackToLintOwnAgentPass`, `TestQARunIntentIsCommitDerivedAndNonAuthoritative`,
`TestQARunNeverScansLocalTranscripts`, `TestReportModeApprovesRebaseConflictAndValidatesUnrebasedHead`.

**S17 — `StartQARun` + `qa run`** · *depends: S11, S13, S16* `↳`
`StartQARun`, `qa_run` IPC through `refuseNested`, the CLI command with the `--mode fix-pr` consent line
requiring `--yes`, `resolveRepo` + `--repo` on the existing surfaces.
*Done when:* `no-mistakes qa run --repo x --mode report` completes and its findings are in the DB.
*Tests:* `TestQAMutationsRefuseNestedGateContext`, `TestPushReceivedIgnoresRepoQAModeAndRunsAllNineSteps`,
`TestReportModeApproveKeepsStepFindingsAndRoundIntact`, `TestReportModeNeverStartsAFixRound`.

**S18 — `internal/qareport` + verdict + `qa report`** · *depends: S17* `↳` · **← milestone**
Report build from persisted rows only, verdict derivation, the post-run hook in `autoCaptureEvalCase`'s
position, `report.json`/`report.md`, `qa_verdict`, retention.
*Tests:* `TestQAReportHookRunsBeforeWorktreeRemoval`, `TestQAReportPanicDoesNotFailAFinishedRun`,
`TestReportModeRunWithBlockingFindingsCompletesWithFindingsVerdict`, `TestQAVerdictIsNilForGateRuns`,
`TestBranchsyncTerminalLogicUnaffectedByQAVerdict`, **e2e `TestQAReportJourney`**.

### Stage E — unattended triggers

**S19 — Discovery + `qa sweep`** · *depends: S18* `↳`
`internal/watch/discover.go`, `watch_state` advancement, backoff. Manual synchronous sweep only — **no
background loop yet**, so discovery is debuggable before anything runs on a timer.
*Tests:* `TestWatchStateNotAdvancedWhenRunFails`, `TestWatchStateNotAdvancedWhenHeadMovedDuringRun`,
`TestMatchRefDoesNotFallBackToBasename`, `TestMatchRefSingleStarDoesNotCrossSlash`.

**S20 — Scheduler + dispatchers + `qa watch *`** · *depends: S19* `↳`
`schedule.go` (single timer, `NextFire`, `MissedAnchor`), the dispatcher pool and queue, daemon
start/stop position, `qa watch add|list|rm`, `qa status`, PR derivation via `FindPR`.
*Tests:* injected `Starter`/`Fetcher` + synthetic clock.

### Stage F — publication and follow-ups

**S21 — `scm.ReviewCommenter`** · *depends: S18*
GitHub impl, `(watchID, headSHA)` marker, upsert, every fail-closed rule. **Start by recording a `gh`
fixture, not by trusting a flag list.**
*Tests:* `TestQAReportCommentIsIdempotentAcrossTwoSweepsOfTheSameHead`,
`TestQAReportCommentPostsNothingWhenFindCommentErrors`, `TestQAReportCommentFailureDoesNotFailTheRun`.

**S22 — `scm.PRLister`** · *depends: S21* — GitHub `ListOpenPRs`; fork-sourced PR discovery.

**S23 — Eval provenance gap** · *depends: S6* — `eval/replay.go:278` builds `&db.Repo{ID:"eval", …}` with
no `SourceName`, so a replayed connected run grades under **different agent conditions than the original**.
Record the resolved boolean on the case and reconstruct it. Independent of Stages D–F; land it any time
after S6.

**Deferred — unattended `fix-pr`.** Needs fork routing, which `validateForkRouting` makes GitHub-only and
`AGENTS.md` puts GitLab/Bitbucket out of scope.

### Migration-order note

The `idx_repos_source_identity` unique index shipped in S1 must remain **last** in `migrationStatements`
and never move to `schemaSQL`. Any spec that appends a column has to append **after** it, which is the one
cross-spec ordering constraint in the whole plan.
*Test:* `TestUniqueSourceIdentityIndexAppliesToUpgradedDatabases`.

---

## S8 — detailed spec: connected gate provisioning

### Context

S8 is the first spec that can **create** a connected repository. Everything in Stage A existed to make
this safe: the guards refuse author-side surfaces, the config block declares the inventory, and the floor
suppresses untrusted project instructions. Nothing has been able to produce a `source_kind='connected'`
row until now except a test inserting one by hand.

### Already in place from S1 — do not rewrite

`internal/gate/connected.go` (83 lines) already has `RemoteIdentity`, `ConnectedRepoID`, and
`ProvisionConnectedGate` (a one-line delegation to `provisionGateCore`). S8 adds the orchestration around
them.

### What S8 adds

**`ensureIdentityStub(ctx, dir, spec)`** — `git init` at `<NM_HOME>/sources/<repoID>`, then pin
`user.name`/`user.email` **`--local`**. No remotes, no refs, no objects, ever.

This must **fail closed when no identity can be resolved.** Verified: `git.CopyLocalUserIdentity`
(`git.go:570`) reads `--local user.name`/`user.email` and `continue`s on empty, so an unpinned stub makes
every run worktree silently fall back to the daemon's *global* git identity — which on a headless host
often does not exist, surfacing as an opaque failure deep in a fix-commit path. Resolution order:
`RepoSpec.CommitName`/`CommitEmail` → the daemon's own global git config → **error**.

The stub being refless is also what makes the S3 guards fail safe:
`GetConfiguredRemoteURLs(stub, "origin")` errors, so local discovery can never overwrite the config
authority even if a guard were missed.

**`EnsureConnected(ctx, d, p, spec)`** — the orchestrator, in this order:
1. resolve identity via `RemoteIdentity` (keys off `identity != ""`, since an operator URL routinely
   carries a token and `inspectRefreshRemote` returns a non-nil error while still populating identity);
2. **collision refusal** — `ConnectedRepoID(identity)`, then refuse if `GetRepo(id)` exists with a
   different identity or is `source_kind='local'` (a 48-bit collision against a path hash). Never silently
   reuse or overwrite. This is also where S4's deferred **duplicate-identity** check lands: two spec names
   resolving to one identity is refused here, against the live DB;
3. `ProvisionConnectedGate`;
4. `ensureIdentityStub`;
5. insert via `InsertConnectedRepo` (already exists) or refresh via `UpdateConnectedRepoSpec` (already
   exists, and already clears `detached_at`);
6. default branch via `git.DefaultBranch(ctx, p.RepoDir(id), "origin")` — its `ls-remote --symref` works
   from the bare gate through `git.RunBare`.

On failure **before** the insert, remove only what this call created, mirroring `InitWithFork`'s
`existing == nil` discipline. Never tear down an already-registered gate on a repair failure.

The URL persisted by `InsertConnectedRepo` must be `safeurl.Redact`ed — the credential belongs only on the
gate's own `origin`. This is invariant C1, whose enforcement is S9.

### Tests

`TestEnsureIdentityStubFailsWhenNoCommitIdentityResolvable` (finding #3),
`TestEnsureIdentityStubHasNoRemotesRefsOrObjects`, `TestConnectedRepoIDIsStableAcrossTransports`,
`TestRemoteIdentityAcceptsCredentialBearingHTTPSRemote`, `TestEnsureConnectedRefusesIDCollision`,
`TestEnsureConnectedRefusesTwoNamesWithOneIdentity`,
`TestEnsureConnectedIsIdempotentAndPreservesHistory`,
`TestEnsureConnectedStoresRedactedUpstreamURL`,
`TestEnsureConnectedCleansUpOnlyWhatItCreatedOnFailure`.

Fixtures use a local bare repo in a temp dir as the "remote", the pattern `internal/gate`'s existing tests
already use.

### Verification

`gofmt -w .` → `make lint` → `go test -race ./internal/gate/... ./internal/db/...` → `go test -race ./...`
→ `go build`. The existing `internal/gate` suite passing unchanged is the evidence that the local
`InitWithFork` path is untouched.

### ⚠ Constraint discovered in S8 — affects S11, S19, and e2e

**A connected repository's URL must be a scheme-qualified or `host:path` remote.** Empirically probed:
`RemoteIdentity` resolves `https://…`, `git@host:path`, and `ssh://…`, but returns `invalid remote` for
both a filesystem path (`/tmp/upstream.git`) **and** `file:///tmp/upstream.git` — `inspectRefreshRemote`'s
scheme switch allows only http/https/ssh/git.

S8 is unaffected: registration never fetches, so its tests use a well-formed but unreachable URL and set
`default_branch` explicitly to avoid an `ls-remote` over the network. But the plan's stated test strategy
for S10 (`repos reconcile` "against a local bare remote") and everything from S11/S19 onward **does** need
a fetchable remote. Options when it bites, in preference order:

1. add `file` to the scheme switch in `inspectRefreshRemote` — small, but that function is shared with the
   local refresh path, so it needs its own regression test that a `file://` clone remote still behaves;
2. serve the temp bare repo over `git daemon` in the e2e harness;
3. keep fetch-dependent coverage e2e-only against a real remote.

Decide at S11 rather than now — the choice depends on whether unit tests there need a real fetch or can
inject a `Fetcher`.

### Two corrections found while implementing S8

- **The duplicate-identity check cannot live in `EnsureConnected`.** Seeing one spec at a time, a second
  name pointing at an already-registered remote is indistinguishable from a *rename*, and renaming is
  explicitly supported (it is what preserves the gate and history). Only a caller holding the whole spec
  set can tell them apart, so this obligation moves again — from S4, to S8, and now to **S10's
  `ReconcileConnected`**. `EnsureConnected` still refuses the case it *can* decide: an identity already
  registered under a different ID.
- S8's `EnsureConnected` is where S4's deferred **identity resolution** landed as planned; only the
  duplicate-name half moved on.

---

## Sequencing for S9–S23

Each remaining spec is planned when it is reached, not all upfront. S4 is the reason: its design changed
on contact with the real dependency graph, and a spec written five steps ahead would have been rewritten
anyway. Order and dependencies are already fixed in [Specs](#specs); what is *not* fixed is the internal
design of each, which depends on what the preceding spec actually produced.

Every spec follows the same loop: read the real call sites → write failing tests → implement → `gofmt`,
`make lint`, targeted `-race`, full `-race` sweep → commit → push to PR #1. A spec whose design deviates
from the plan on contact gets the deviation recorded in its commit message and in this file, as S4's did.

---

## S3 — detailed spec: author surfaces refuse connected repos

### Context

Every author-side surface assumes a developer checkout: a cwd inside a registered clone, an exact
checked-out branch, a local default branch to compare against. A connected repo has none of that — its
`working_path` is a refless identity stub under `<NM_HOME>/sources/<id>` (finding #2). Those surfaces must
refuse it rather than operate on the stub.

This lands **before** S8, the first spec that can create a connected repo. While no registration path
exists the guards are unreachable in production and testable only by hand-inserting a row — which is the
point: no later bug can reach a live `branchsync` mutation path, because the refusals were already there
when the path became reachable.

Ships **no new capability**. Every change is a refusal, a display string, or an early `return nil`.

### Two corrections to §8, both verified

§8 claims the `findRepo` choke point covers `init`, `eject`, `stats`, and the wizard. It does not:

- `findRepo` (`cli/root.go:111`) has exactly **six** callers — `sync.go:99`, `runs.go:27`, `attach.go:38`,
  `rerun.go:32`, `status.go:30`, `axi.go:132`. `init` and `eject` reach `gate.Init`/`gate.Eject` with a
  `workDir` and never call it; `stats` aggregates every repo through `getRepos()`.
- So **layer 4 below is not optional cleanup** — it is the only thing standing between a connected repo and
  `init`/`eject`/`stats`.

Also verified: `openSyncService` (`cli/sync.go:94`) already routes through `findRepo`, so §8's separate
entry for it needs no work beyond layer 1.

### Layer 1 — the `findRepo` choke point

`cli/root.go:111`. Refuse at **both** successful return points (`:120` and the main-worktree fallback at
`:132`), so a linked worktree cannot slip past. One shared error naming the repo by `SourceName` and
pointing at `repos show`. Covers all six callers above.

### Layer 2 — `branchsync.OpenCurrent`, the verified hole (finding #6)

`internal/branchsync/sync.go:149`. Confirmed: it does its own `GetRepoByPath` at `:163` with the same
main-worktree fallback, then returns a fully wired `Service` at `:178` with `Apply`/`Recover` live — it
never consults `findRepo`. Refuse after repo resolution and **before** constructing the `Service`, closing
the DB on the way out like the existing failure paths.

This disarms the TUI with **no TUI change**, because `tui/app.go:493` is already
`if service, closeFn, err := branchsync.OpenCurrent(); err == nil`.

### Layer 3 — `branchsync` self-guards

Defense in depth for any future caller that constructs a `Service` directly, as `cli/status.go:65` and
`cli/axi_drive.go:259` both do today.

- New `StateNotApplicable = "not_applicable"` beside the state constants (`sync.go:24-48`), with no
  `NextAction`, so `recover_custody` and `user_owned` are never offered for a repo with no local branch.
- New `connectedRefusal() (State, bool)` **mirroring the existing `gateContextRefusal`**
  (`sync.go:323`) — same signature, same call position, same "no files or refs were changed" phrasing.
  That helper is the precedent; do not invent a second shape.
- Call it first in `inspect` (`sync.go:912`), `Apply` (`sync.go:348`), and `Recover` (`sync.go:508`).
  `Apply`/`Recover` assert independently rather than trusting derived state, per this repo's
  self-guarding convention — they are the only worktree-mutating entry points.
- **`CanApply` (`sync.go:124`) stays untouched:** it tests `Safety`, and `StateNotApplicable` carries an
  empty `Safety`, so `CanApply` is already false by construction. Adding a case there would be a second
  owner of the same question.

### Layer 4 — gate, rebase, and stats

- **`gate.Eject`** (`gate.go:309`) — refuse a connected repo, pointing at `repos remove`. Insert directly
  after the existing `gatecontext` nested check at `:310`, matching that shape.
- **`gate.InitWithFork`** (`gate.go:52`) — refuse roots under `SourcesDir()`, `ReposDir()`, and
  `WorktreesDir()`. **Verified pre-existing gap:** the only `paths` use in the whole function is
  `p.ReposDir()` passed to `provisionGate` at `:127`; nothing today stops `init` inside a gate or worktree.
  Insert after the `gatecontext` check at `:53`, before `FindMainRepoRoot`.
- **`steps/rebase.go detectBundledLocalDefaultCommits`** (`:185`) — return `nil` when
  `sctx.Repo.Connected()`, beside the existing empty-`workingPath` guard at `:189`. Benign on a refless
  stub today, but it produces a **blocking, non-auto-fixable** finding and must not depend on the stub
  staying refless.
- **`db/stats.go`** — `RepoStats` (`:31`) gains `SourceName`, populated at `:60` (`getRepos` already
  selects it through the shared `repoColumns`/`scanRepo` that S1 widened); `DisplayName()` (`:41`) prefers
  it over `filepath.Base(WorkingPath)`, which for a connected repo prints the bare hex ID.

### Files

`internal/cli/root.go` · `internal/branchsync/sync.go` · `internal/gate/gate.go` ·
`internal/pipeline/steps/rebase.go` · `internal/db/stats.go` — plus test files. No TUI change, no
`cli/sync.go`/`status.go`/`axi_drive.go` change.

### Tests

Each needs a hand-inserted connected row (`InsertRepoWithIDAndFork` then
`UpdateRepoSourceBinding`, per `db/watch_test.go`) and, for the cwd-sensitive ones, `t.Chdir` into a stub
created with `git init`.

- `TestBranchSyncOpenCurrentRefusesConnectedRepoFromStubCWD` — **the headline test** (finding #6).
- `TestFindRepoRefusesConnectedRepoFromStubCWD`, and the same via the main-worktree fallback.
- `TestBranchSyncApplyAndRecoverRefuseConnectedRepoIndependently` — assert **no** ref or file changed.
- `TestBranchSyncInspectReportsNotApplicableWithoutNextAction`.
- `TestCanApplyIsFalseForNotApplicableState`.
- `TestEjectRefusesConnectedRepo`, `TestInitRefusesRootsUnderDaemonOwnedDirectories` (three cases).
- `TestDetectBundledLocalDefaultCommitsSkipsConnectedRepos`.
- `TestRepoStatsDisplayNamePrefersSourceName`.
- **Regression guard:** the whole existing `internal/branchsync`, `internal/gate`, and `internal/cli`
  suites must pass **unchanged** — a local repo's `SourceKind` is `'local'`, so every guard is a no-op for
  it. That is the real proof this ships no behaviour change.

### Verification

`gofmt -w .` → `make lint` → `go test -race ./internal/branchsync/... ./internal/gate/... ./internal/cli/...
./internal/db/... ./internal/pipeline/steps/...` → `go test -race ./...` → `make e2e` →
`go build -o ./bin/no-mistakes ./cmd/no-mistakes`.

`make e2e` matters here specifically: `internal/branchsync` and the `axi` journeys are the suites most
likely to catch an over-broad refusal that also blocks local repos.

---

## Verification

Per `AGENTS.md`: `gofmt -w .` → `make lint` → `go test -race ./...` → `make e2e` →
`go build -o ./bin/no-mistakes ./cmd/no-mistakes`.

**Security / data-safety (ship-blocking):**
`TestGlobalSourceYAMLRedactsConnectedRepoCredentials`,
`TestStoredStepRoundGlobalConfigNeverContainsUserinfo` (finding #1 — **do not ship Phase 3 without
both**), `TestRefreshRepoURLsRefusesConnectedRepos`,
`TestRefreshConnectedRepoURLsWritesRedactedRowBeforeGateRemote` (C1 + write order),
`TestRepoConfigCannotChangeQAPolicy` and `TestRepoConfigCannotDeclareConnectedRepos` (mirroring
`TestRepoConfigCannotChangeEvalCollection`), `TestOperatorPolicyCannotClearTrustedDisableProjectSettings`,
`TestReconcileConnectedNeverDeletesAnyRepoOrHistory`, `TestReconcileConnectedNeverTouchesLocalRepos`,
`TestQAMutationsRefuseNestedGateContext`.

**Gate-path preservation (the regression guard):** `TestGatePolicyNilPreservesBlockingGate`,
`TestGatePolicyIsNeverInstalledForGateRuns`,
`TestPushReceivedIgnoresRepoQAModeAndRunsAllNineSteps`,
`TestQARunConcurrencyCapNeverDelaysAPush`, `TestQARunDoesNotSupersedeAnActiveGateRun`,
`TestBranchLockStillSerializesConcurrentStartsForSameBranch`.

**Unattended correctness:** `TestReportModeApproveKeepsStepFindingsAndRoundIntact`,
`TestReportModeRunNeverSetsAwaitingAgentSince`, `TestPolicyResolvedGateRejectsIPCRespond`,
`TestPolicyIsNotConsultedForReconcilableCIGate`, `TestFixPRPolicyFixesOnceThenApproves`,
`TestRecoveredQARunReinstallsItsGatePolicyAndSkipSet`,
`TestReportModeApprovesRebaseConflictAndValidatesUnrebasedHead`,
`TestQARunIntentIsCommitDerivedAndNonAuthoritative`, `TestQARunNeverScansLocalTranscripts`,
`TestReportModeNeverStartsAFixRound`.

**Structure and semantics:** `TestSkipSetForIsExhaustiveOverModes` (table test that fails when a new
mode appears without a declared set), `TestReportAndCommentSkipSetsAreIdentical`,
`TestValidateRecoveredRunAcceptsQARunWithSkippedSteps`,
`TestRunSkippedStepsColumnMatchesSkippedStepResults`,
`TestSkippedDocumentStepFallsBackToLintOwnAgentPass`,
`TestReportModeRunWithBlockingFindingsCompletesWithFindingsVerdict`,
`TestQAVerdictIsNilForGateRuns`, `TestBranchsyncTerminalLogicUnaffectedByQAVerdict`,
`TestMatchRefDoesNotFallBackToBasename` (`MatchRef("refs/heads/feature/main","main") == false`),
`TestMatchRefSingleStarDoesNotCrossSlash`,
`TestUniqueSourceIdentityIndexAppliesToUpgradedDatabases` (pins the migration ordering),
`TestBranchLocksDoNotGrowUnbounded`, `TestDaemonStopIsNotBlockedByReportModeQARuns`,
`TestDaemonStopIsBlockedByFixPRQARun`, `TestEnsureIdentityStubFailsWhenNoCommitIdentityResolvable`,
`TestBranchSyncOpenCurrentRefusesConnectedRepoFromStubCWD`,
`TestConnectedRepoIDIsStableAcrossTransports`,
`TestRemoteIdentityAcceptsCredentialBearingHTTPSRemote`,
`TestDetachedConnectedRepoReattachesWithSameIDAndHistory`,
`TestWatchStateNotAdvancedWhenRunFails`, `TestWatchStateNotAdvancedWhenHeadMovedDuringRun`,
`TestQAReportHookRunsBeforeWorktreeRemoval`, `TestQAReportPanicDoesNotFailAFinishedRun`,
`TestQAReportCommentIsIdempotentAcrossTwoSweepsOfTheSameHead`,
`TestQAReportCommentPostsNothingWhenFindCommentErrors`,
`TestQAReportCommentFailureDoesNotFailTheRun`.

**End-to-end (behind the `e2e` tag):** `TestQAReportJourney` — register a connected repo from a local
bare "remote" in a temp dir, push a commit to it, `qa run --mode report`, assert a report with findings
is produced, that **no ref on the remote moved**, and that the operator's own local gates are untouched.

**Manual:** a throwaway GitHub repo → `repos reconcile` → `qa run --repo x --branch main --mode report`
→ inspect the report → confirm `git ls-remote` shows the remote unchanged.

---

## Open items and known limits

- **Cron** needs a third-party library; `poll_interval` + `nightly_at` ships without one. Raise before
  adding any dependency, per `AGENTS.md`.
- **`max_concurrent=2` / `max_total_runs=4` / `15m` / `6h` are reasoned, not measured.** Instrument
  `qa status` and revisit.
- **No documentation findings in `report`/`comment` mode**, because `document` is skipped. The real fix
  is an `sctx.ReadOnly` flag suppressing `commitAgentFixes`, deliberately out of scope.
- **No in-pipeline bound on a `fix-pr` QA run's CI park.** `qa.max_run_duration` is a watcher-side
  backstop, not a fix; the honest fix is a `ci.qa_timeout` floor, not added because it would multiply
  CI-timeout owners.
- **Fork-sourced PRs are invisible until Phase 8**, since `FindPR`-by-branch only sees branches present
  in the gate.
- **Identity path lowercasing** can alias two repos differing only in case on a case-sensitive forge.
  The unique index turns that into a visible refusal rather than silent aliasing, but it is a real
  limit of reusing the existing normalizer. Document it.
- **`assertGateTrustedConfigReadable` still aborts** when the trusted default branch cannot be fetched,
  even if the floor already forces `disable_project_settings`. So a connected repo with an unreachable
  default branch can never be QA'd. Failing closed is correct; not relaxed here.
- **The generated skill** describes only the gate. Keep it gate-only unless you want agents driving QA
  runs; if so, that is a separate skill section plus a drift-test update (`make skill`, never edit
  `SKILL.md`).

---
