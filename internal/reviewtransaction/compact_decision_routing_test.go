package reviewtransaction

import (
	"strings"
	"testing"
)

// TestCompleteReview_FlagOffAllInconclusive_RoutesToEscalated proves the
// default-off feature flag preserves the legacy all-inconclusive route.
func TestCompleteReview_FlagOffAllInconclusive_RoutesToEscalated(t *testing.T) {
	state, err := completeReviewRoutingFixture(t, "flag-off", false,
		[]Finding{decisionRoutingFinding("R3-INCONCLUSIVE")},
		[]FindingEvidence{decisionRoutingClassification("R3-INCONCLUSIVE")},
		[]EvidenceResult{decisionRoutingOutcome("R3-INCONCLUSIVE", OutcomeInconclusive)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateEscalated || state.DecisionRequiredEnabled {
		t.Fatalf("routing = state %q, flag %t; want escalated with flag disabled", state.State, state.DecisionRequiredEnabled)
	}
}

// TestCompleteReview_FlagOnAllInconclusive_RoutesToDecisionRequired proves an
// enabled flag pauses an all-inconclusive review deterministically.
func TestCompleteReview_FlagOnAllInconclusive_RoutesToDecisionRequired(t *testing.T) {
	findings := []Finding{decisionRoutingFinding("R3-INCONCLUSIVE")}
	classifications := []FindingEvidence{decisionRoutingClassification("R3-INCONCLUSIVE")}
	outcomes := []EvidenceResult{decisionRoutingOutcome("R3-INCONCLUSIVE", OutcomeInconclusive)}
	first, err := completeReviewRoutingFixture(t, "flag-on-first", true, findings, classifications, outcomes)
	if err != nil {
		t.Fatal(err)
	}
	second, err := completeReviewRoutingFixture(t, "flag-on-replay", true, findings, classifications, outcomes)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != StateDecisionRequired || !first.DecisionRequiredEnabled {
		t.Fatalf("routing = state %q, flag %t; want decision_required with flag enabled", first.State, first.DecisionRequiredEnabled)
	}
	first.LineageID, second.LineageID = "replay", "replay"
	if firstRevision, _ := CompactRevisionForState(first); firstRevision == "" {
		t.Fatal("first replay produced an empty revision")
	} else if secondRevision, _ := CompactRevisionForState(second); secondRevision != firstRevision {
		t.Fatalf("replay revision = %q, want %q", secondRevision, firstRevision)
	}
}

// TestCompleteReview_FlagOnMixedOutcome_RoutesToEscalated proves one
// conclusive severe outcome prevents the decision-required route.
func TestCompleteReview_FlagOnMixedOutcome_RoutesToEscalated(t *testing.T) {
	state, err := completeReviewRoutingFixture(t, "mixed", true,
		[]Finding{decisionRoutingFinding("R3-INCONCLUSIVE"), decisionRoutingFinding("R3-REFUTED")},
		[]FindingEvidence{decisionRoutingClassification("R3-INCONCLUSIVE"), decisionRoutingClassification("R3-REFUTED")},
		[]EvidenceResult{
			decisionRoutingOutcome("R3-INCONCLUSIVE", OutcomeInconclusive),
			decisionRoutingOutcome("R3-REFUTED", OutcomeRefuted),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateEscalated {
		t.Fatalf("mixed routing state = %q, want %q", state.State, StateEscalated)
	}
}

// TestCompleteReview_MalformedInput_RoutesToEscalated preserves the v1
// consumed-terminal contract: malformed refuter output returns an error but
// leaves a valid, persistable escalated authority rather than pausing it.
func TestCompleteReview_MalformedInput_RoutesToEscalated(t *testing.T) {
	state, err := completeReviewRoutingFixture(t, "malformed", true,
		[]Finding{decisionRoutingFinding("R3-MALFORMED")},
		[]FindingEvidence{decisionRoutingClassification("R3-MALFORMED")},
		[]EvidenceResult{decisionRoutingOutcome("R3-MALFORMED", EvidenceOutcome("malformed"))},
	)
	if err == nil || !strings.Contains(err.Error(), "unsupported refuter outcome") {
		t.Fatalf("malformed completion error = %v, want unsupported refuter outcome", err)
	}
	if state.State != StateEscalated || state.Outcomes["R3-MALFORMED"] != OutcomeInconclusive {
		t.Fatalf("malformed routing = state %q, outcome %q; want escalated/inconclusive", state.State, state.Outcomes["R3-MALFORMED"])
	}
}

// TestCompleteReview_FlagOnZeroInconclusive_RoutesToApproved proves the flag
// leaves the conclusive happy path unchanged: CompleteReview enters validating,
// then successful verification reaches approved without a decision pause.
func TestCompleteReview_FlagOnZeroInconclusive_RoutesToApproved(t *testing.T) {
	state, err := completeReviewRoutingFixture(t, "zero-inconclusive", true,
		[]Finding{decisionRoutingFinding("R3-REFUTED")},
		[]FindingEvidence{decisionRoutingClassification("R3-REFUTED")},
		[]EvidenceResult{decisionRoutingOutcome("R3-REFUTED", OutcomeRefuted)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != StateValidating {
		t.Fatalf("complete-review state = %q, want %q before final verification", state.State, StateValidating)
	}
	if err := state.CompleteVerification([]byte("decision routing focused tests passed\n"), true); err != nil {
		t.Fatal(err)
	}
	if state.State != StateApproved {
		t.Fatalf("verified routing state = %q, want %q", state.State, StateApproved)
	}
}

func completeReviewRoutingFixture(t *testing.T, suffix string, enabled bool, findings []Finding, classifications []FindingEvidence, outcomes []EvidenceResult) (CompactState, error) {
	t.Helper()
	repo := initSnapshotRepo(t)
	writeSnapshotFile(t, repo, "tracked.txt", "candidate\n")
	state := newCompactTestState(t, repo, "decision-routing-"+suffix)
	if len(state.SelectedLenses) != 1 {
		t.Fatalf("routing fixture selected lenses = %v, want one", state.SelectedLenses)
	}
	lens := strings.TrimPrefix(state.SelectedLenses[0], "review-")
	for index := range findings {
		findings[index].Lens = lens
	}
	store, err := CompactAuthoritativeStore(t.Context(), repo, state.LineageID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.Replace("", "review/start", state)
	if err != nil {
		t.Fatal(err)
	}
	completionErr := state.CompleteReview(CompactReviewInput{
		LensResults:             []LensResult{{Lens: state.SelectedLenses[0], Findings: findings, Evidence: []string{"reviewed exact candidate"}}},
		Classifications:         classifications,
		RefuterOutcomes:         outcomes,
		DecisionRequiredEnabled: enabled,
	})
	if _, err := store.Replace(revision, "review/complete-review", state); err != nil {
		t.Fatalf("persist routed state: %v (completion error: %v)", err, completionErr)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return loaded.State, completionErr
}

func decisionRoutingFinding(id string) Finding {
	return Finding{ID: id, Location: "tracked.txt:1", Severity: "CRITICAL", Claim: "candidate behavior remains uncertain", ProofRefs: []string{"focused routing evidence"}}
}

func decisionRoutingClassification(id string) FindingEvidence {
	return FindingEvidence{FindingID: id, Class: EvidenceInferential, Causality: CausalIntroduced, Proof: "candidate-only trace requires interpretation"}
}

func decisionRoutingOutcome(id string, outcome EvidenceOutcome) EvidenceResult {
	return EvidenceResult{FindingID: id, Outcome: outcome, Proof: "independent refuter trace"}
}
