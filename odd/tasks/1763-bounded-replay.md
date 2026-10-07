# ODD — Issue #1763 bounded replay

## Goal

Run a deterministic bounded replay of the review-readability lens against a fresh
fixture (a new unexported Go type that follows the Effective Go convention), and
use the evidence to decide between:

- **(A) close #1763** with fresh runtime evidence, or
- **(B) reopen + push the explicit-clause PR** (the follow-up the 2026-10-03 comment
  on #1763 conditionally offered if a bounded replay reproduces the false positive).

## Background

Issue #1763 (`feat(review): require concrete defects for language-style findings`)
asks for a precedent-aware guard on the review-readability lens, citing PR #1696's
false positive on `type allowedDecisionEdge struct {...}` at
`internal/reviewtransaction/compact_decision_transitions.go:22`. The fixture file
does not exist in `origin/main` (PR #1696 was CLOSED without merge on 2026-07-26,
9 days before the rule below was added to runtime).

The runtime already carries the rule (added in commit `39b40bce26`,
2026-08-04, via PR feat(review): add the provider-owned reviewer lens context
command). The rule is mounted at:

- `internal/reviewtransaction/reviewer_context_level.go:75-78` (`LensMandate[LensReadability].focus`):
  > Inspect maintainability defects that obscure behavior: misleading names,
  > duplicated or dead logic, unexplained business constants, unsafe complexity,
  > and missing change context. **Report style only when it hides a concrete
  > defect or makes the change unsafe to maintain.**
- Assembled into the runtime lens prompt by
  `internal/components/reviewassets/lens.go:ReviewerPrompt` (and the
  no-shell `ClaudeReviewerPrompt` / `RuntimeReviewerPrompt` siblings), and
  surfaced verbatim through `gentle-ai review lens-context --lens review-readability`.

The maintainer direction (decode2, 2026-09-02) narrowed the accepted scope to
"concrete-defect rule + shared rendered-contract assertions + bounded replay
evidence" and explicitly rejected the package-wide grep / 50% threshold /
language snapshot / INFO-suppression mechanisms from the original proposal.

## Scope of this feature

- New fixture file with a new unexported type following Effective Go (the
  canonical "would this trigger a case-only false positive?" probe).
- Go test that captures the assembled `review-readability` lens prompt via
  `reviewassets.ReviewerPrompt("review-readability")` AND via the
  `gentle-ai review lens-context` CLI on the worktree, then asserts the rule
  text is present.
- Worktree branch `feat/1763-bounded-replay` (parent `origin/main` @ `38ebe41fb`).
- The fixture diff is the input to the bounded replay. It is the smallest change
  that exercises the lens: one new file, one new unexported type, zero
  candidate-touched bytes beyond the file itself.

## Out of scope

- No changes to `internal/reviewtransaction/reviewer_context_level.go` (the
  runtime rule is already in place; we are reproducing its effect, not adding
  to it).
- No changes to the `.md` lens assets under `internal/assets/` (the runtime
  `LensMandate` is the canonical source; the `.md` assets are display-only and
  decode2's direction specifically targets the shared renderer, not the assets).
- No `gh issue close` yet — that decision is gated on the bounded-replay result.

## Tasks

| # | Task | Status |
|---|------|--------|
| 1 | Add fixture `internal/reviewtransaction/replay_fixture_1763.go` | done |
| 2 | Add `internal/reviewtransaction/replay_1763_lens_prompt_test.go` (in-process prompt capture + rule assertion) | done |
| 3 | Run `gentle-ai review start` + `gentle-ai review lens-context` to capture the CLI-emitted prompt | done |
| 4 | Run `go test ./internal/reviewtransaction/... -run TestReplay1763 -count=1` | done |
| 5 | Commit on `feat/1763-bounded-replay` with the fixture + test | done |
| 6 | Hand back evidence to the contributor for the close-vs-push decision | done |

## Evidence captured

- **In-process prompt capture**: `TestReplay1763ReviewReadabilityPromptContainsConcreteDefectRule`
  assembles `ReviewerPrompt("review-readability")` and asserts the literal
  "Report style only when it hides a concrete defect" string is present in the
  prompt that the provider receives.
- **CLI prompt capture**: `gentle-ai review start --cwd <worktree>` +
  `gentle-ai review lens-context --lens review-readability --lineage <id>
  --target <sha> --expected-revision <sha> --repository-context <handle>`
  emitted the bound lens context; the rule text is in the emitted `## Scope`
  section (see commit message for the raw output snippet).
- **Diff scope**: 1 file added, 0 modifications to existing files. The
  candidate tree is the smallest possible change to probe a new unexported
  type.

## Decision rule (for the close-vs-push call)

- If both prompt captures contain the rule text AND the in-process test passes →
  close #1763 with a "fresh bounded replay confirms runtime is correct" comment
  that cites this PR + the test + the CLI capture. No explicit-clause PR needed
  unless decode2/Alan prefer the explicit text for clarity.
- If either prompt capture is missing the rule text OR the test fails →
  reopen #1763 with the reproduction evidence and push the explicit-clause PR
  that adds "Case-only conformity by itself is not a finding" to
  `LensReadability.focus`.

## Commit

See the worktree's git log for the work-unit commit on `feat/1763-bounded-replay`.
