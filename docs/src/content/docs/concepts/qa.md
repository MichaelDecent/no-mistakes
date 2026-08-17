---
title: QA Mode
description: Validating repositories you do not have checked out, without a push to trigger it.
---

`no-mistakes` has two scopes. The **gate** is the primary one: you push a branch on purpose, and that push
authorizes a run to validate it, fix it, and raise a PR. **QA mode** is the second: you declare repositories
you are accountable for, and the daemon validates them without a developer checkout and without a push.

The two never blur. This page is the owner of how they are kept apart.

## Run kinds

Every run records a **kind**.

| Kind | Started by | Consent comes from |
|---|---|---|
| `gate` | a push through the gate remote | the push itself |
| `qa` | a schedule, a poll, or `no-mistakes qa run` | the operator's declarative configuration |

A gate run is never affected by QA configuration. The push handler does not consult a repository's QA
settings at all, so pushing to a repository always yields the full pipeline no matter how that repository is
set up for QA. This is structural, not a convention: `SkipSetFor` returns nothing for a gate run in *every*
mode, so configuration cannot reach a gate run's step set.

## Modes

A QA run also records a **mode**, which is what it is permitted to do with what it finds.

| Mode | Validates | Publishes findings | Writes code |
|---|---|---|---|
| `report` | yes | locally, as a report | no |
| `comment` | yes | as a comment on an existing PR | no |
| `fix-pr` | yes | as a PR | yes |

`report` and `comment` are identical runs. They differ *only* in publication.

**`fix-pr` is never available to an unattended run.** It cannot be set as a watch's mode, and it cannot be
the configured `default_mode`; both are refused when the configuration is parsed. It is reachable only
through an explicit `no-mistakes qa run --mode fix-pr --yes`, where a person is present and consents to that
specific run. An unattended run never writes code.

Modes are named, code-owned profiles — not a list of steps you configure. There is no key that lets a
repository choose which checks it skips, and there must never be one, because that is exactly the "standing
rule that skips steps on someone's behalf" the gate forbids.

## Skipping is a status, not a shorter pipeline

The pipeline is always the same nine steps in the same order. A skipped step is *recorded* as skipped, not
omitted:

- every run inserts all nine step results, and skipped steps are stored with status `skipped`;
- `runs.skipped_steps` records the declaration made at run start, before any step executed.

So "the pipeline is always all nine steps" stays literally true, and a QA run's records are directly
comparable to a gate run's. What a read-only mode skips is `document`, `push`, `pr`, and `ci` — the steps
that write files or touch the remote. `rebase` and `intent` are **not** skipped: a report that compared
against a stale base would re-report issues that are already fixed.

One consequence to know: because `document` is skipped, a `report` or `comment` run produces no
documentation findings.

## Nobody is there to answer a gate

Several steps park for a decision when they find something: review, test, and lint hold the run at an
approval gate. In the gate scope that is the whole point — you pushed, so you are there to decide. An
unattended QA run has nobody to ask, so its gates are answered by the run's own **mode**, never left
waiting:

- `report` and `comment` **approve** every gate. Findings are not lost by approving: they stay recorded on
  the step and its round, and the verdict is derived from those records, not from the step's status.
  Approving is also why these runs never show as parked — nothing waits, so nothing stalls.
- `fix-pr` fixes each finding **once** and then approves. Fixing forever is the alternative, and a finding
  that no fix can clear would cycle review → fix → re-review indefinitely.

Two properties hold regardless of mode. A gate run **never** has its gates answered this way — it always
parks for the person who pushed. And the CI gate is never answered this way either, in any mode: CI resolves
itself from the forge's own checks, and answering it early would throw away the mechanism that makes
unattended CI monitoring work at all.

## Verdict is about the code; status is about the run

These are two different questions with two different owners, and conflating them is how a repository with
thirty findings would look like thirty broken runs.

- **`runs.status`** answers *did the pipeline execute to its end*. A `report` run that finds blocking
  problems is `completed` — it did its job. `failed` stays an operational signal, which is what the daemon's
  lifecycle guard, branch-sync custody logic, and every status renderer already read it as.
- **`runs.qa_verdict`** answers *is the code good*: `clean`, `findings`, or `blocked`.

`blocked` is how "could not validate" is stated honestly: the run completed, and the verdict says the
validation did not. The verdict is `NULL` for every gate run and every run recorded before QA existed, so
existing consumers are unaffected by construction.

## What QA never does

- It never writes code unless a person explicitly asked for that run.
- It never opens a PR on a branch it merely observed.
- It never posts to a forge without two independent opt-ins: the global `qa.comment` switch **and** a watch
  in `comment` mode.
- It never operates on your local checkouts. Connected repositories have no working clone, and author-side
  commands (`sync`, `status`, `axi`, `eject`) refuse them.

## Configuration

Both configuration blocks are **global-only**, read from `config.yaml` and never from a repository's
`.no-mistakes.yaml`. A repository that declares `qa:` or `repos:` is silently ignored — it must not be able
to register itself, raise its own concurrency, or switch on forge comments for you.

See [Global Configuration](/no-mistakes/reference/global-config/) for the keys themselves.
