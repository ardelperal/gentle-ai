package reviewtransaction

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// stubAdjudicator is a configurable test double that records every call and
// returns the pre-set outcome (and optional error). It is the test
// implementation of DecisionAdjudicator for the bounded-action contract:
// the action must invoke Adjudicate exactly once per Execute, and a
// retriable provider error must surface as ErrAdjudicatorUnavailable.
type stubAdjudicator struct {
	outcome DecisionAdjudicationOutcome
	err     error
	calls   int
	severe  [][]Finding
}

func (s *stubAdjudicator) Adjudicate(_ context.Context, severe []Finding) (DecisionAdjudicationOutcome, error) {
	s.calls++
	s.severe = append(s.severe, append([]Finding(nil), severe...))
	return s.outcome, s.err
}

// decisionAdjudicateCarryOnFixture persists a low-risk lineage directly into
// StateDecisionCarryOn via a real CompactStore round-trip. The fixture
// pattern mirrors review_decide_test.go: a low-risk lineage with no
// selected lenses and no findings lets Validate() accept the StateDecisionCarryOn
// successor even though the state machine's `unresolved` rule (compact.go
// line 605) normally blocks DecisionCarryOn whenever severe findings are
// inconclusive. The adjudicator is invoked once with an empty severe
// slice; the stub decides the outcome.
func decisionAdjudicateCarryOnFixture(t *testing.T, repo, lineage string) (CompactState, CompactStore) {
	t.Helper()
	reviewed := filepath.Join(repo, "docs", "decision-required.md")
	if err := os.MkdirAll(filepath.Dir(reviewed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewed, []byte("decision-adjudicate candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	builder := SnapshotBuilder{Repo: repo}
	snapshot, err := builder.Build(context.Background(), Target{
		Kind: TargetCurrentChanges, IntendedUntracked: []string{"docs/decision-required.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	risk, lines, err := builder.ClassifySnapshotRisk(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewCompactState(Start{
		LineageID: lineage, Mode: ModeOrdinaryBounded, Generation: 1,
		Snapshot: snapshot, PolicyHash: "sha256:" + strings.Repeat("dc", 32), RiskLevel: risk,
		SelectedLenses: []string{}, OriginalChangedLines: &lines,
	})
	if err != nil {
		t.Fatal(err)
	}
	state.State = StateDecisionCarryOn
	state.DecisionRequiredEnabled = true
	state.Decision = &DecisionPayload{
		Operation:  CompactDecideOperation,
		LineageID:  lineage,
		Decision:   "continue",
		Revision:   "sha256:" + strings.Repeat("ab", 32),
		RecordedBy: "adjudicate-fixture@example.com",
		RecordedAt: time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC),
	}
	if err := state.Validate(); err != nil {
		t.Fatalf("fixture Validate() = %v", err)
	}
	store, err := CompactAuthoritativeStore(context.Background(), repo, lineage)
	if err != nil {
		t.Fatal(err)
	}
	writeCompactFixtureRecord(t, store, state)
	if err := os.Remove(reviewed); err != nil {
		t.Fatal(err)
	}
	return state, store
}

// TestDecisionAdjudicateBatch_InvokesOnceOnUnresolved is the single-invocation
// guarantee from spec §5.2 (WARNING #2 mitigation). The action must invoke
// the adjudicator exactly once; an internal loop or batched call would
// reproduce the defect that killed #1433 by re-triggering the routing
// branch from inside the consumer.
func TestDecisionAdjudicateBatch_InvokesOnceOnUnresolved(t *testing.T) {
	repo := initSnapshotRepo(t)
	state, store := decisionAdjudicateCarryOnFixture(t, repo, "adjudicate-once")
	stub := &stubAdjudicator{outcome: DecisionAdjudicationAllResolved}

	action := &DecisionAdjudicationAction{
		Adjudicator:     stub,
		AdjudicatorName: "stub-once",
	}
	result, err := action.Execute(context.Background(), store)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("adjudicator calls = %d, want 1 (bounded-action contract)", stub.calls)
	}
	if result.TargetState != StateApproved {
		t.Fatalf("target state = %q, want %q for all_resolved", result.TargetState, StateApproved)
	}
	if result.LineageID != state.LineageID {
		t.Fatalf("result lineage = %q, want %q", result.LineageID, state.LineageID)
	}
	if result.PreviousState != StateDecisionCarryOn {
		t.Fatalf("previous state = %q, want %q", result.PreviousState, StateDecisionCarryOn)
	}
}

// TestDecisionAdjudicateBatch_AllResolved_RoutesToApproved covers edge #6
// from the canonical truth table: an adjudicator verdict of all_resolved
// routes the lineage to StateApproved. The result's TargetState is the
// planned successor; the caller is responsible for committing it via
// ReplaceContext.
func TestDecisionAdjudicateBatch_AllResolved_RoutesToApproved(t *testing.T) {
	repo := initSnapshotRepo(t)
	state, store := decisionAdjudicateCarryOnFixture(t, repo, "adjudicate-all-resolved")
	stub := &stubAdjudicator{outcome: DecisionAdjudicationAllResolved}

	action := &DecisionAdjudicationAction{
		Adjudicator:     stub,
		AdjudicatorName: "stub-all-resolved",
	}
	result, err := action.Execute(context.Background(), store)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.TargetState != StateApproved {
		t.Fatalf("target state = %q, want %q", result.TargetState, StateApproved)
	}
	if result.Outcome != DecisionAdjudicationAllResolved {
		t.Fatalf("outcome = %q, want %q", result.Outcome, DecisionAdjudicationAllResolved)
	}
	if result.Next.State != StateApproved {
		t.Fatalf("next.State = %q, want %q", result.Next.State, StateApproved)
	}
	if result.Next.Decision == nil {
		t.Fatal("next.Decision = nil, want populated with adjudication payload")
	}
	if result.Next.Decision.Decision != "continue" {
		t.Fatalf("next.Decision.Decision = %q, want %q (user decision must carry forward)", result.Next.Decision.Decision, "continue")
	}
	if result.Next.Decision.Adjudication == nil {
		t.Fatal("next.Decision.Adjudication = nil, want bounded adjudication payload")
	}
	if result.Next.Decision.Adjudication.Operation != CompactDecisionAdjudicateBatchOperation {
		t.Fatalf("adjudication operation = %q, want %q", result.Next.Decision.Adjudication.Operation, CompactDecisionAdjudicateBatchOperation)
	}
	if result.Next.Decision.Adjudication.Adjudicator != "stub-all-resolved" {
		t.Fatalf("adjudication adjudicator = %q, want %q", result.Next.Decision.Adjudication.Adjudicator, "stub-all-resolved")
	}
	_ = state
}

// TestDecisionAdjudicateBatch_UnresolvedRemain_RoutesToValidating covers
// edge #5 from the canonical truth table: an adjudicator verdict of
// unresolved_remain routes the lineage to StateValidating so the
// correction flow re-runs with refreshed evidence.
func TestDecisionAdjudicateBatch_UnresolvedRemain_RoutesToValidating(t *testing.T) {
	repo := initSnapshotRepo(t)
	_, store := decisionAdjudicateCarryOnFixture(t, repo, "adjudicate-unresolved")
	stub := &stubAdjudicator{outcome: DecisionAdjudicationUnresolvedRemain}

	action := &DecisionAdjudicationAction{
		Adjudicator:     stub,
		AdjudicatorName: "stub-unresolved",
	}
	result, err := action.Execute(context.Background(), store)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.TargetState != StateValidating {
		t.Fatalf("target state = %q, want %q", result.TargetState, StateValidating)
	}
	if result.Outcome != DecisionAdjudicationUnresolvedRemain {
		t.Fatalf("outcome = %q, want %q", result.Outcome, DecisionAdjudicationUnresolvedRemain)
	}
	if result.Next.State != StateValidating {
		t.Fatalf("next.State = %q, want %q", result.Next.State, StateValidating)
	}
}

// TestDecisionAdjudicateBatch_NoneResolved_RoutesToEscalated covers edge
// #7 from the canonical truth table: an adjudicator verdict of
// none_resolved escalates the lineage because no severe finding was
// resolvable.
func TestDecisionAdjudicateBatch_NoneResolved_RoutesToEscalated(t *testing.T) {
	repo := initSnapshotRepo(t)
	_, store := decisionAdjudicateCarryOnFixture(t, repo, "adjudicate-none")
	stub := &stubAdjudicator{outcome: DecisionAdjudicationNoneResolved}

	action := &DecisionAdjudicationAction{
		Adjudicator:     stub,
		AdjudicatorName: "stub-none",
	}
	result, err := action.Execute(context.Background(), store)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.TargetState != StateEscalated {
		t.Fatalf("target state = %q, want %q", result.TargetState, StateEscalated)
	}
	if result.Outcome != DecisionAdjudicationNoneResolved {
		t.Fatalf("outcome = %q, want %q", result.Outcome, DecisionAdjudicationNoneResolved)
	}
	if result.Next.State != StateEscalated {
		t.Fatalf("next.State = %q, want %q", result.Next.State, StateEscalated)
	}
}

// TestDecisionAdjudicateBatch_ProviderFailure_Retriable covers scenario
// #4 from spec §5.3: a retriable provider error from the adjudicator
// surfaces as ErrAdjudicatorUnavailable and leaves the lineage untouched.
func TestDecisionAdjudicateBatch_ProviderFailure_Retriable(t *testing.T) {
	repo := initSnapshotRepo(t)
	_, store := decisionAdjudicateCarryOnFixture(t, repo, "adjudicate-failure")
	providerErr := errors.New("provider connection refused")
	stub := &stubAdjudicator{outcome: "", err: providerErr}

	action := &DecisionAdjudicationAction{
		Adjudicator:     stub,
		AdjudicatorName: "stub-failure",
	}
	result, err := action.Execute(context.Background(), store)
	if err == nil {
		t.Fatal("execute error = nil, want ErrAdjudicatorUnavailable")
	}
	if !errors.Is(err, ErrAdjudicatorUnavailable) {
		t.Fatalf("execute error = %v, want ErrAdjudicatorUnavailable", err)
	}
	if stub.calls != 1 {
		t.Fatalf("adjudicator calls = %d, want 1 (bounded-action contract on failure)", stub.calls)
	}
	if result.TargetState != "" {
		t.Fatalf("target state = %q, want empty on retriable failure", result.TargetState)
	}
	// Verify the lineage was NOT mutated by reloading from the store.
	reloaded, err := store.LoadContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State.State != StateDecisionCarryOn {
		t.Fatalf("lineage state after failure = %q, want %q (must remain on retriable error)", reloaded.State.State, StateDecisionCarryOn)
	}
}

// TestDecisionAdjudicateBatch_WrongStateRejected covers scenario #5 from
// spec §5.3: invoking the consumer on a lineage that is not in
// StateDecisionCarryOn returns ErrIllegalStateForAdjudicate and the
// adjudicator is NOT invoked.
func TestDecisionAdjudicateBatch_WrongStateRejected(t *testing.T) {
	repo := initSnapshotRepo(t)
	// Persist a StateDecisionRequired lineage directly so we exercise the
	// wrong-source path; the adjudicator must never be called.
	_, store := decisionRequiredCarryOnWrongStateFixture(t, repo, "adjudicate-wrong-state")
	stub := &stubAdjudicator{outcome: DecisionAdjudicationAllResolved}

	action := &DecisionAdjudicationAction{
		Adjudicator:     stub,
		AdjudicatorName: "stub-wrong",
	}
	_, err := action.Execute(context.Background(), store)
	if err == nil {
		t.Fatal("execute error = nil, want ErrIllegalStateForAdjudicate")
	}
	if !errors.Is(err, ErrIllegalStateForAdjudicate) {
		t.Fatalf("execute error = %v, want ErrIllegalStateForAdjudicate", err)
	}
	if stub.calls != 0 {
		t.Fatalf("adjudicator calls = %d, want 0 (must not be invoked on wrong state)", stub.calls)
	}
}

// TestDecisionAdjudicateBatch_DoesNotMutateFrozenFields proves spec §5.2's
// preservation rule: the consumer's computed successor must leave LensResults,
// Findings, Classifications, Outcomes, FixFindingIDs, FollowUps, and the
// snapshot/tier/budget fields byte-identical to the predecessor. Only
// State (and Decision.Adjudication) may differ.
func TestDecisionAdjudicateBatch_DoesNotMutateFrozenFields(t *testing.T) {
	repo := initSnapshotRepo(t)
	_, store := decisionAdjudicateCarryOnFixture(t, repo, "adjudicate-frozen")
	stub := &stubAdjudicator{outcome: DecisionAdjudicationAllResolved}

	predecessor, err := store.LoadContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	action := &DecisionAdjudicationAction{
		Adjudicator:     stub,
		AdjudicatorName: "stub-frozen",
	}
	result, err := action.Execute(context.Background(), store)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !reflect.DeepEqual(predecessor.State.LensResults, result.Next.LensResults) {
		t.Fatalf("LensResults mutated: predecessor=%v next=%v", predecessor.State.LensResults, result.Next.LensResults)
	}
	if !reflect.DeepEqual(predecessor.State.Findings, result.Next.Findings) {
		t.Fatalf("Findings mutated: predecessor=%v next=%v", predecessor.State.Findings, result.Next.Findings)
	}
	if !reflect.DeepEqual(predecessor.State.Classifications, result.Next.Classifications) {
		t.Fatalf("Classifications mutated: predecessor=%v next=%v", predecessor.State.Classifications, result.Next.Classifications)
	}
	if !reflect.DeepEqual(predecessor.State.Outcomes, result.Next.Outcomes) {
		t.Fatalf("Outcomes mutated: predecessor=%v next=%v", predecessor.State.Outcomes, result.Next.Outcomes)
	}
	if !reflect.DeepEqual(predecessor.State.FixFindingIDs, result.Next.FixFindingIDs) {
		t.Fatalf("FixFindingIDs mutated: predecessor=%v next=%v", predecessor.State.FixFindingIDs, result.Next.FixFindingIDs)
	}
	if !reflect.DeepEqual(predecessor.State.FollowUps, result.Next.FollowUps) {
		t.Fatalf("FollowUps mutated: predecessor=%v next=%v", predecessor.State.FollowUps, result.Next.FollowUps)
	}
	if !snapshotsEqual(predecessor.State.InitialSnapshot, result.Next.InitialSnapshot) {
		t.Fatalf("InitialSnapshot mutated")
	}
	if !snapshotsEqual(predecessor.State.CurrentSnapshot, result.Next.CurrentSnapshot) {
		t.Fatalf("CurrentSnapshot mutated")
	}
	if predecessor.State.PolicyHash != result.Next.PolicyHash {
		t.Fatalf("PolicyHash mutated: predecessor=%q next=%q", predecessor.State.PolicyHash, result.Next.PolicyHash)
	}
	if predecessor.State.RiskLevel != result.Next.RiskLevel {
		t.Fatalf("RiskLevel mutated: predecessor=%q next=%q", predecessor.State.RiskLevel, result.Next.RiskLevel)
	}
	if !equalStrings(predecessor.State.SelectedLenses, result.Next.SelectedLenses) {
		t.Fatalf("SelectedLenses mutated: predecessor=%v next=%v", predecessor.State.SelectedLenses, result.Next.SelectedLenses)
	}
	if predecessor.State.OriginalChangedLines != result.Next.OriginalChangedLines {
		t.Fatalf("OriginalChangedLines mutated: predecessor=%d next=%d", predecessor.State.OriginalChangedLines, result.Next.OriginalChangedLines)
	}
	if predecessor.State.CorrectionBudget != result.Next.CorrectionBudget {
		t.Fatalf("CorrectionBudget mutated: predecessor=%d next=%d", predecessor.State.CorrectionBudget, result.Next.CorrectionBudget)
	}
	if predecessor.State.LineageID != result.Next.LineageID {
		t.Fatalf("LineageID mutated: predecessor=%q next=%q", predecessor.State.LineageID, result.Next.LineageID)
	}
	if predecessor.State.Generation != result.Next.Generation {
		t.Fatalf("Generation mutated: predecessor=%d next=%d", predecessor.State.Generation, result.Next.Generation)
	}
	// Decision.Decision literal must carry forward unchanged.
	if result.Next.Decision == nil || result.Next.Decision.Decision != "continue" {
		t.Fatalf("user decision literal not preserved: next.Decision = %#v", result.Next.Decision)
	}
}

// decisionRequiredCarryOnWrongStateFixture persists a lineage in
// StateDecisionRequired directly so the wrong-state rejection test has a
// non-DecisionCarryOn source state. Mirrors the WU4 fixture pattern.
func decisionRequiredCarryOnWrongStateFixture(t *testing.T, repo, lineage string) (CompactState, CompactStore) {
	t.Helper()
	reviewed := filepath.Join(repo, "docs", "decision-required-wrong.md")
	if err := os.MkdirAll(filepath.Dir(reviewed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewed, []byte("decision-required wrong-state candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	builder := SnapshotBuilder{Repo: repo}
	snapshot, err := builder.Build(context.Background(), Target{
		Kind: TargetCurrentChanges, IntendedUntracked: []string{"docs/decision-required-wrong.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	risk, lines, err := builder.ClassifySnapshotRisk(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewCompactState(Start{
		LineageID: lineage, Mode: ModeOrdinaryBounded, Generation: 1,
		Snapshot: snapshot, PolicyHash: "sha256:" + strings.Repeat("dd", 32), RiskLevel: risk,
		SelectedLenses: []string{}, OriginalChangedLines: &lines,
	})
	if err != nil {
		t.Fatal(err)
	}
	state.State = StateDecisionRequired
	state.DecisionRequiredEnabled = true
	if err := state.Validate(); err != nil {
		t.Fatalf("fixture Validate() = %v", err)
	}
	store, err := CompactAuthoritativeStore(context.Background(), repo, lineage)
	if err != nil {
		t.Fatal(err)
	}
	writeCompactFixtureRecord(t, store, state)
	if err := os.Remove(reviewed); err != nil {
		t.Fatal(err)
	}
	return state, store
}
