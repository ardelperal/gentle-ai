package reviewtransaction

import (
	"context"
	"fmt"
	"time"
)

// DecisionAdjudicationOutcome enumerates the three results the bounded
// evidence-adjudicator provider may report. The literals are the externally
// visible contract that the action, the journal, and any downstream audit
// share so they can branch on the adjudicator's verdict without re-reading
// the lineage. Mutating any of these strings would silently shift every
// persisted journal entry's discriminator, so the constants are pinned here.
type DecisionAdjudicationOutcome string

const (
	// DecisionAdjudicationAllResolved: the adjudicator resolved every severe
	// finding the user authorized for the carry-on invocation. Edge #6 from
	// the canonical truth table (engram obs #21219) routes the lineage to
	// StateApproved when all severe findings are no longer inconclusive.
	DecisionAdjudicationAllResolved DecisionAdjudicationOutcome = "all_resolved"

	// DecisionAdjudicationUnresolvedRemain: at least one severe finding
	// remains inconclusive after the adjudicator's single invocation. Edge
	// #5 routes the lineage to StateValidating so the correction flow re-runs
	// with the refreshed evidence.
	DecisionAdjudicationUnresolvedRemain DecisionAdjudicationOutcome = "unresolved_remain"

	// DecisionAdjudicationNoneResolved: the adjudicator could not resolve any
	// of the authorized severe findings. Edge #7 escalates the lineage.
	DecisionAdjudicationNoneResolved DecisionAdjudicationOutcome = "none_resolved"
)

// DecisionAdjudicator is the bounded evidence-adjudicator provider contract.
// The action invokes Adjudicate exactly once per Execute call; the provider
// MUST NOT batch or retry internally because the bounded-action guarantee
// (spec §5.2) depends on a single, observable invocation. A retriable
// provider error is mapped to ErrAdjudicatorUnavailable by the action and
// surfaced to the caller without an auto-loop.
type DecisionAdjudicator interface {
	// Adjudicate runs the bounded adjudication on the named severe findings.
	// Returns one of DecisionAdjudicationAllResolved, ...UnresolvedRemain,
	// ...NoneResolved on success, or a non-nil error on retriable failure.
	// The error is wrapped by the action as ErrAdjudicatorUnavailable so
	// callers can classify the failure mode with errors.Is.
	Adjudicate(ctx context.Context, severe []Finding) (DecisionAdjudicationOutcome, error)
}

// DecisionAdjudicationAction is the forward-path consumer for the v2
// decision-required state machine (issue #1380). The bounded action executes
// exactly one adjudicator call and journals the planned transition; the
// caller is responsible for committing the resulting CompactState via
// store.ReplaceContext using the predecessor revision CAS. Splitting the
// planning step from the commit keeps the consumer free of the
// replace-store-lock critical section and lets the caller inspect, defer,
// or replay the planned successor without mutating the authority revision.
//
// The bounded-action contract is the single most load-bearing property of
// this type: re-entering the action, looping inside Execute, or batching
// the adjudicator call would each reproduce the WARNING #2 defect from the
// proposal (spec §5.2) by re-triggering the routing branch from inside the
// consumer.
type DecisionAdjudicationAction struct {
	// Adjudicator is the bounded provider the action invokes once. Required.
	Adjudicator DecisionAdjudicator

	// AdjudicatorName is recorded in the journal and the
	// DecisionAdjudicatePayload so a downstream audit can trace which
	// provider answered the bounded call. Empty is allowed for tests but
	// production callers must set a non-empty identifier.
	AdjudicatorName string

	// RequestDigest binds the journal entry to the finalize-attempt request
	// that owns this consumer invocation. When empty the journal write is
	// skipped (useful for unit tests that exercise the bounded-action
	// contract without standing up a finalize attempt).
	RequestDigest string

	// Now overrides the recorded timestamp in tests; zero falls back to
	// time.Now().UTC at execution time. Production callers leave this zero.
	Now time.Time
}

// DecisionAdjudicationResult is the bounded output of one Execute call. The
// caller uses Next (with the computed TargetState) to commit the transition
// via store.ReplaceContext using PreviousRevision as the CAS expectation.
// Journaled reports whether the planned transition was durably recorded in
// the finalize-attempt journal; when false the caller is responsible for
// retrying or surfacing the loss to its operator.
type DecisionAdjudicationResult struct {
	// LineageID echoes the lineage the action ran against.
	LineageID string

	// PreviousState is the state observed at the start of the bounded
	// action; always StateDecisionCarryOn on a non-error result.
	PreviousState State

	// PreviousRevision is the authority revision observed at the start of
	// the bounded action. The caller passes this as the --expected-revision
	// CAS on the subsequent ReplaceContext call.
	PreviousRevision string

	// TargetState is the state selected by the truth-table edge that the
	// adjudicator outcome triggered (obs #21219 edges 5, 6, 7).
	TargetState State

	// TargetRevision is the durable revision the journal entry recorded for
	// the planned successor. The caller verifies this matches the
	// ReplaceContext return value on the commit attempt.
	TargetRevision string

	// Outcome is the verbatim adjudicator verdict, recorded in the journal
	// payload's Adjudicator field for downstream auditing.
	Outcome DecisionAdjudicationOutcome

	// AdjudicatorName echoes the action's provider identifier for the audit.
	AdjudicatorName string

	// AdjudicatedAt is the UTC timestamp the bounded call returned at.
	AdjudicatedAt time.Time

	// EvidenceHashes lists the severe findings the adjudicator was invoked
	// against (their IDs), in the iteration order of the predecessor's
	// Findings slice. Useful for tests that assert what scope the bounded
	// call covered.
	EvidenceHashes []string

	// Next is the computed successor. Only the State field differs from the
	// predecessor; every other field is byte-identical per spec §5.2.
	// callers inspect Next before committing it via ReplaceContext.
	Next CompactState

	// Journaled reports whether the planned transition was durably recorded
	// via RecordFinalizeAttemptTransition. False means the caller must
	// retry the journal write or surface the loss.
	Journaled bool
}

// Execute runs the bounded action: one adjudicator call + one journal
// entry. The flow is:
//
//  1. Load the current compact record from store.
//  2. Reject non-DecisionCarryOn states with ErrIllegalStateForAdjudicate.
//  3. Collect the unresolved severe findings.
//  4. Invoke the adjudicator exactly once (the bounded-action guarantee).
//  5. Map the outcome to the target state per obs #21219:
//     all_resolved       -> StateApproved     (edge 6)
//     unresolved_remain  -> StateValidating   (edge 5)
//     none_resolved      -> StateEscalated    (edge 7)
//  6. Build the successor (only State differs from the predecessor per
//     spec §5.2; frozen fields are preserved byte-identical).
//  7. Compute the target revision and journal the planned transition via
//     RecordFinalizeAttemptTransition when RequestDigest is set.
//
// Execute does NOT commit the transition; store.ReplaceContext is the
// caller's job. This separation lets the caller inspect the planned
// successor (or hold it for replay) without immediately mutating the
// authority revision.
func (a *DecisionAdjudicationAction) Execute(ctx context.Context, store CompactStore) (DecisionAdjudicationResult, error) {
	record, err := store.LoadContext(ctx)
	if err != nil {
		return DecisionAdjudicationResult{}, fmt.Errorf("load decision-adjudicate lineage: %w", err)
	}
	if record.State.State != StateDecisionCarryOn {
		return DecisionAdjudicationResult{}, fmt.Errorf("%w: got %q", ErrIllegalStateForAdjudicate, record.State.State)
	}

	severe := unresolvedSevereFindings(record.State)

	outcome, err := a.Adjudicator.Adjudicate(ctx, severe)
	if err != nil {
		return DecisionAdjudicationResult{}, fmt.Errorf("%w: %v", ErrAdjudicatorUnavailable, err)
	}

	target, err := mapDecisionAdjudicationOutcomeToState(outcome)
	if err != nil {
		return DecisionAdjudicationResult{}, err
	}

	adjudicatedAt := a.recordedAt()
	next := record.State
	next.State = target
	if next.Decision != nil {
		// Carry the user's continue/stop decision forward unchanged and
		// attach the bounded adjudication payload as a nested field. The
		// validateCompactSuccessor arm allows Decision to differ because
		// the adjudicator-set Adjudication field is the bounded call's
		// external contract (mirroring the WU4 review/decide pattern).
		payload := *next.Decision
		payload.Adjudication = &DecisionAdjudicatePayload{
			Operation:      CompactDecisionAdjudicateBatchOperation,
			LineageID:      record.State.LineageID,
			Adjudicator:    a.AdjudicatorName,
			EvidenceHashes: decisionAdjudicationEvidenceHashes(severe),
			RetryCount:     0,
			DecidedAt:      adjudicatedAt,
		}
		next.Decision = &payload
	}

	targetRevision, err := CompactRevisionForState(next)
	if err != nil {
		return DecisionAdjudicationResult{}, fmt.Errorf("compute target revision: %w", err)
	}

	journaled := false
	if a.RequestDigest != "" {
		if err := store.RecordFinalizeAttemptTransition(a.RequestDigest, CompactDecisionAdjudicateBatchOperation, targetRevision); err != nil {
			return DecisionAdjudicationResult{}, fmt.Errorf("record decision-adjudicate transition: %w", err)
		}
		journaled = true
	}

	return DecisionAdjudicationResult{
		LineageID:        record.State.LineageID,
		PreviousState:    record.State.State,
		PreviousRevision: record.Revision,
		TargetState:      target,
		TargetRevision:   targetRevision,
		Outcome:          outcome,
		AdjudicatorName:  a.AdjudicatorName,
		AdjudicatedAt:    adjudicatedAt,
		EvidenceHashes:   decisionAdjudicationEvidenceHashes(severe),
		Next:             next,
		Journaled:        journaled,
	}, nil
}

// recordedAt returns the action's override timestamp when set, or the
// current UTC time otherwise. The override exists so deterministic tests
// can pin the recorded timestamp without freezing the wall clock.
func (a *DecisionAdjudicationAction) recordedAt() time.Time {
	if a.Now.IsZero() {
		return time.Now().UTC()
	}
	return a.Now.UTC()
}

// mapDecisionAdjudicationOutcomeToState selects the truth-table target
// state for one adjudicator outcome. The mapping is the canonical
// interpretation of edges 5, 6, 7 from obs #21219: all_resolved ->
// Approved, unresolved_remain -> Validating, none_resolved -> Escalated.
// An unrecognized outcome is rejected so a caller cannot accidentally
// route to a state the truth table does not list.
func mapDecisionAdjudicationOutcomeToState(outcome DecisionAdjudicationOutcome) (State, error) {
	switch outcome {
	case DecisionAdjudicationAllResolved:
		return StateApproved, nil
	case DecisionAdjudicationUnresolvedRemain:
		return StateValidating, nil
	case DecisionAdjudicationNoneResolved:
		return StateEscalated, nil
	default:
		return "", fmt.Errorf("decision-adjudicate-batch: unsupported outcome %q", outcome)
	}
}

// unresolvedSevereFindings collects the severe findings whose evidence
// class or causality forces an inconclusive outcome. The slice is the
// input the adjudicator is authorized to re-examine under the bounded
// carry-on. The helper is intentionally local to the consumer so the
// carry-on scope stays bounded by the lineage's frozen finding set and
// never widens beyond what the user authorized with --decision continue.
func unresolvedSevereFindings(state CompactState) []Finding {
	out := make([]Finding, 0)
	for _, finding := range state.Findings {
		if !isSevereSeverity(finding.Severity) {
			continue
		}
		outcome, ok := state.Outcomes[finding.ID]
		if !ok || outcome != OutcomeInconclusive {
			continue
		}
		out = append(out, finding)
	}
	return out
}

// decisionAdjudicationEvidenceHashes captures the severe findings the
// bounded call covered as their IDs, in the iteration order of the
// predecessor's Findings slice. The IDs are the audit-stable identifiers
// the journal entry binds to; using IDs (rather than full finding
// payloads) keeps the journal entry's bytes bounded by the carry-on scope.
func decisionAdjudicationEvidenceHashes(findings []Finding) []string {
	hashes := make([]string, len(findings))
	for index, finding := range findings {
		hashes[index] = finding.ID
	}
	return hashes
}
