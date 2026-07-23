package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gentleman-programming/gentle-ai/internal/reviewtransaction"
)

// decisionRequiredCLIRecord persists one compact lineage directly into the
// v2 store and returns the revision. The state is built in DecisionRequired
// so the decide command can transition or reject it under test. The optional
// pre-populated decision payload lets idempotency tests reuse the fixture.
func decisionRequiredCLIRecord(t *testing.T, repo, lineage string, payload *reviewtransaction.DecisionPayload) (revision string) {
	t.Helper()
	reviewed := filepath.Join(repo, "docs", "decision-required.md")
	if err := os.MkdirAll(filepath.Dir(reviewed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewed, []byte("decision-required candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	builder := reviewtransaction.SnapshotBuilder{Repo: repo}
	snapshot, err := builder.Build(context.Background(), reviewtransaction.Target{
		Kind: reviewtransaction.TargetCurrentChanges, IntendedUntracked: []string{"docs/decision-required.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	risk, lines, err := builder.ClassifySnapshotRisk(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	state, err := reviewtransaction.NewCompactState(reviewtransaction.Start{
		LineageID: lineage, Mode: reviewtransaction.ModeOrdinaryBounded, Generation: 1,
		Snapshot: snapshot, PolicyHash: "sha256:" + strings.Repeat("dc", 32), RiskLevel: risk,
		SelectedLenses: []string{}, OriginalChangedLines: &lines,
	})
	if err != nil {
		t.Fatal(err)
	}
	state.State = reviewtransaction.StateDecisionRequired
	state.DecisionRequiredEnabled = true
	state.Decision = payload
	revision = writeReconcileCLIRecord(t, repo, state)
	if err := os.Remove(reviewed); err != nil {
		t.Fatal(err)
	}
	return revision
}

// decisionRequiredCLIPayload constructs a payload that marks the lineage as
// already having a recorded decision. The recorded revision is informational
// only; idempotency matches on the decision literal, and the user-supplied
// --expected-revision is the live CAS the decide command enforces.
func decisionRequiredCLIPayload(lineage, decision string) *reviewtransaction.DecisionPayload {
	return &reviewtransaction.DecisionPayload{
		Operation:  reviewtransaction.CompactDecideOperation,
		LineageID:  lineage,
		Decision:  decision,
		Revision:  "sha256:" + strings.Repeat("ab", 32),
		RecordedBy: "rec-fixture@example.com",
		RecordedAt: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
	}
}

func decideCLIArgs(repo, lineage, revision, decision, reason string) []string {
	return []string{
		"decide", "--cwd", repo, "--lineage", lineage,
		"--expected-revision", revision, "--decision", decision, "--reason", reason,
	}
}

// TestRunReviewDecide_StopMovesToEscalated proves that --decision stop
// transitions a DecisionRequired lineage to Escalated.
func TestRunReviewDecide_StopMovesToEscalated(t *testing.T) {
	repo := initReviewCLIRepo(t)
	revision := decisionRequiredCLIRecord(t, repo, "decide-stop-escalated", nil)

	var output bytes.Buffer
	err := RunReview(decideCLIArgs(repo, "decide-stop-escalated", revision, "stop", "operator chose stop"), &output)
	if err != nil {
		t.Fatalf("review decide stop: %v\n%s", err, output.String())
	}
	var result ReviewDecideResult
	decodeStrictReviewJSON(t, output.Bytes(), &result)
	if result.Operation != "review/decide" ||
		result.LineageID != "decide-stop-escalated" ||
		result.State != reviewtransaction.StateEscalated ||
		result.Decision != "stop" ||
		result.Idempotent {
		t.Fatalf("decide result = %#v", result)
	}

	store, err := reviewtransaction.CompactAuthoritativeStore(context.Background(), repo, "decide-stop-escalated")
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if record.State.State != reviewtransaction.StateEscalated {
		t.Fatalf("persisted state = %q, want %q", record.State.State, reviewtransaction.StateEscalated)
	}
	if record.State.Decision == nil || record.State.Decision.Decision != "stop" {
		t.Fatalf("persisted decision payload = %#v", record.State.Decision)
	}
}

// TestRunReviewDecide_ContinueMovesToCarryOn proves that --decision continue
// transitions a DecisionRequired lineage to DecisionCarryOn.
func TestRunReviewDecide_ContinueMovesToCarryOn(t *testing.T) {
	repo := initReviewCLIRepo(t)
	revision := decisionRequiredCLIRecord(t, repo, "decide-continue-carryon", nil)

	var output bytes.Buffer
	err := RunReview(decideCLIArgs(repo, "decide-continue-carryon", revision, "continue", "operator chose continue"), &output)
	if err != nil {
		t.Fatalf("review decide continue: %v\n%s", err, output.String())
	}
	var result ReviewDecideResult
	decodeStrictReviewJSON(t, output.Bytes(), &result)
	if result.State != reviewtransaction.StateDecisionCarryOn ||
		result.Decision != "continue" ||
		result.Idempotent {
		t.Fatalf("decide result = %#v", result)
	}

	store, err := reviewtransaction.CompactAuthoritativeStore(context.Background(), repo, "decide-continue-carryon")
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if record.State.State != reviewtransaction.StateDecisionCarryOn {
		t.Fatalf("persisted state = %q, want %q", record.State.State, reviewtransaction.StateDecisionCarryOn)
	}
}

// TestRunReviewDecide_RevisionMismatchRejected proves a stale
// --expected-revision is refused with the typed revision-mismatch error
// and the lineage is left untouched.
func TestRunReviewDecide_RevisionMismatchRejected(t *testing.T) {
	repo := initReviewCLIRepo(t)
	revision := decisionRequiredCLIRecord(t, repo, "decide-revision-mismatch", nil)
	statePath := filepath.Join(reviewCLIAuthorityRoot(t, repo), "v2", "decide-revision-mismatch", "review-state.json")
	original, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	stale := "sha256:" + strings.Repeat("0", 64)
	if stale == revision {
		stale = "sha256:" + strings.Repeat("1", 64)
	}
	err = RunReview(decideCLIArgs(repo, "decide-revision-mismatch", stale, "stop", "operator stop"), &bytes.Buffer{})
	if !errors.Is(err, reviewtransaction.ErrAuthorityRevisionMismatch) {
		t.Fatalf("revision-mismatch error = %v, want ErrAuthorityRevisionMismatch", err)
	}

	current, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, current) {
		t.Fatalf("refused decide mutated the state file")
	}
}

// TestRunReviewDecide_WrongStateRejected proves invoking decide on a lineage
// that is not in DecisionRequired is refused with ErrIllegalStateForDecide.
func TestRunReviewDecide_WrongStateRejected(t *testing.T) {
	repo := initReviewCLIRepo(t)
	_, store := approveDiscoveryMarkdown(t, repo, "decide-wrong-state", "docs/wrong.md", "wrong\n")
	record, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(reviewCLIAuthorityRoot(t, repo), "v2", "decide-wrong-state", "review-state.json")
	original, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	err = RunReview(decideCLIArgs(repo, "decide-wrong-state", record.Revision, "stop", "operator stop"), &bytes.Buffer{})
	if !errors.Is(err, reviewtransaction.ErrIllegalStateForDecide) {
		t.Fatalf("wrong-state error = %v, want ErrIllegalStateForDecide", err)
	}

	current, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, current) {
		t.Fatalf("refused decide mutated the state file")
	}
}

// TestRunReviewDecide_IdempotentStop proves re-applying the same --decision
// stop with the same revision returns the previously recorded payload and
// leaves the lineage in DecisionRequired (truth-table edge 2).
func TestRunReviewDecide_IdempotentStop(t *testing.T) {
	repo := initReviewCLIRepo(t)
	lineage := "decide-idempotent-stop"
	revision := decisionRequiredCLIRecord(t, repo, lineage, decisionRequiredCLIPayload(lineage, "stop"))

	var output bytes.Buffer
	if err := RunReview(decideCLIArgs(repo, lineage, revision, "stop", "operator stop"), &output); err != nil {
		t.Fatalf("idempotent decide stop: %v\n%s", err, output.String())
	}
	var result ReviewDecideResult
	decodeStrictReviewJSON(t, output.Bytes(), &result)
	if !result.Idempotent || result.Decision != "stop" || result.State != reviewtransaction.StateDecisionRequired {
		t.Fatalf("idempotent stop result = %#v", result)
	}

	store, err := reviewtransaction.CompactAuthoritativeStore(context.Background(), repo, lineage)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if record.State.State != reviewtransaction.StateDecisionRequired {
		t.Fatalf("idempotent stop persisted state = %q, want %q", record.State.State, reviewtransaction.StateDecisionRequired)
	}
	if record.Revision != revision {
		t.Fatalf("idempotent stop bumped revision = %q, want unchanged %q", record.Revision, revision)
	}
}

// TestRunReviewDecide_IdempotentContinue proves re-applying --decision
// continue with the same revision is idempotent and stays in
// DecisionRequired (truth-table edge 2).
func TestRunReviewDecide_IdempotentContinue(t *testing.T) {
	repo := initReviewCLIRepo(t)
	lineage := "decide-idempotent-continue"
	revision := decisionRequiredCLIRecord(t, repo, lineage, decisionRequiredCLIPayload(lineage, "continue"))

	var output bytes.Buffer
	if err := RunReview(decideCLIArgs(repo, lineage, revision, "continue", "operator continue"), &output); err != nil {
		t.Fatalf("idempotent decide continue: %v\n%s", err, output.String())
	}
	var result ReviewDecideResult
	decodeStrictReviewJSON(t, output.Bytes(), &result)
	if !result.Idempotent || result.Decision != "continue" || result.State != reviewtransaction.StateDecisionRequired {
		t.Fatalf("idempotent continue result = %#v", result)
	}

	store, err := reviewtransaction.CompactAuthoritativeStore(context.Background(), repo, lineage)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if record.State.State != reviewtransaction.StateDecisionRequired {
		t.Fatalf("idempotent continue persisted state = %q, want %q", record.State.State, reviewtransaction.StateDecisionRequired)
	}
}

// TestRunReviewDecide_ConflictingDecisionsRejected proves that re-applying
// decide with a different --decision than the recorded one returns
// ErrDecisionConflict and leaves the lineage untouched.
func TestRunReviewDecide_ConflictingDecisionsRejected(t *testing.T) {
	repo := initReviewCLIRepo(t)
	lineage := "decide-conflict"
	revision := decisionRequiredCLIRecord(t, repo, lineage, decisionRequiredCLIPayload(lineage, "stop"))
	statePath := filepath.Join(reviewCLIAuthorityRoot(t, repo), "v2", lineage, "review-state.json")
	original, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	err = RunReview(decideCLIArgs(repo, lineage, revision, "continue", "operator reversed"), &bytes.Buffer{})
	if !errors.Is(err, reviewtransaction.ErrDecisionConflict) {
		t.Fatalf("conflict error = %v, want ErrDecisionConflict", err)
	}

	current, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, current) {
		t.Fatalf("refused conflicting decide mutated the state file")
	}
}

// TestRunReviewDecide_MalformedDecisionFlagRejected proves an unknown
// --decision literal returns ErrInvalidDecisionFlag without mutating the
// lineage.
func TestRunReviewDecide_MalformedDecisionFlagRejected(t *testing.T) {
	repo := initReviewCLIRepo(t)
	revision := decisionRequiredCLIRecord(t, repo, "decide-malformed-flag", nil)
	statePath := filepath.Join(reviewCLIAuthorityRoot(t, repo), "v2", "decide-malformed-flag", "review-state.json")
	original, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	err = RunReview(decideCLIArgs(repo, "decide-malformed-flag", revision, "pause", "operator pause"), &bytes.Buffer{})
	if !errors.Is(err, reviewtransaction.ErrInvalidDecisionFlag) {
		t.Fatalf("malformed-flag error = %v, want ErrInvalidDecisionFlag", err)
	}

	current, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, current) {
		t.Fatalf("refused malformed-flag decide mutated the state file")
	}
}

// revisionPlaceholder is removed; payloads record an informational revision
// that does not participate in the idempotency match. The user-supplied
// --expected-revision remains the authoritative CAS check.
