package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gentleman-programming/gentle-ai/internal/reviewtransaction"
)

// ReviewDecideSchema is the externally-visible schema literal for the
// review/decide JSON envelope. The constant is the single source of truth
// for every audit and consumer that needs to recognize the result from raw
// JSON; mutating it would silently shift every previously persisted
// envelope's contract.
const ReviewDecideSchema = "gentle-ai.review-decide-result/v1"

// ReviewDecideResult is the JSON envelope emitted by `gentle-ai review
// decide`. The shape is stable: the Idempotent flag distinguishes a true
// replay from a first-time transition so a downstream consumer can branch
// without re-reading the lineage.
type ReviewDecideResult struct {
	Schema     string                             `json:"schema"`
	Operation  string                             `json:"operation"`
	LineageID  string                             `json:"lineage_id"`
	Decision   string                             `json:"decision"`
	Reason     string                             `json:"reason"`
	State      reviewtransaction.State            `json:"state"`
	StoreRev   string                             `json:"store_revision"`
	Idempotent bool                               `json:"idempotent"`
	RecordedBy string                             `json:"recorded_by"`
	RecordedAt time.Time                          `json:"recorded_at"`
	Payload    reviewtransaction.DecisionPayload `json:"payload"`
}

// ReviewIntegrationDecideResult is the negotiated (dotted-form) public
// envelope for the decide operation. It excludes provider-private fields
// (cwd, repository, paths) and pins the schema literal so an audit can
// reconstruct the contract from the persisted bytes alone.
type ReviewIntegrationDecideResult struct {
	Operation     string                  `json:"operation"`
	LineageID     string                  `json:"lineage_id"`
	Decision      string                  `json:"decision"`
	State         reviewtransaction.State `json:"state"`
	StoreRevision string                  `json:"store_revision"`
	Idempotent    bool                    `json:"idempotent"`
}

// ParseReviewDecideFlags wires the standard CLI flag set for the decide
// subcommand. The helper is exported so tests can exercise the flag
// semantics without depending on the higher-level dispatch; the
// production call site is RunReviewDecide.
//
// The --decision literal is restricted to `continue` or `stop`; every
// other value is refused before the lineage is loaded so an invalid
// flag never produces a state-mutating request. --expected-revision is
// the exact CAS the store enforces on commit. --recorded-by defaults to
// the active OS user so the audit record carries the operator without
// requiring an extra flag.
func ParseReviewDecideFlags(name string, stdout io.Writer, args []string) (cwd, lineage, expected, decision, reason, recordedBy string, err error) {
	flags := newReviewFlagSet(name, stdout, "Resolve a decision_required pause with a CAS-protected user decision (continue or stop).")
	cwdFlag := flags.String("cwd", ".", "repository path")
	lineageFlag := flags.String("lineage", "", "review lineage identifier")
	expectedFlag := flags.String("expected-revision", "", "exact current authority revision as sha256:hex; CAS-protected")
	decisionFlag := flags.String("decision", "", "user decision literal: continue or stop")
	reasonFlag := flags.String("reason", "", "human-readable justification for the decision")
	recordedFlag := flags.String("recorded-by", "", "operator identity recorded in the decision payload (defaults to $USER)")
	if err = parseReviewFlags(flags, args); err != nil {
		return
	}
	if reviewHelpRequested(args) {
		return
	}
	if flags.NArg() != 0 {
		err = fmt.Errorf("unexpected review decide argument %q", flags.Arg(0))
		return
	}
	cwd = strings.TrimSpace(*cwdFlag)
	lineage = strings.TrimSpace(*lineageFlag)
	expected = strings.TrimSpace(*expectedFlag)
	decision = strings.TrimSpace(*decisionFlag)
	reason = strings.TrimSpace(*reasonFlag)
	recordedBy = strings.TrimSpace(*recordedFlag)
	if recordedBy == "" {
		recordedBy = strings.TrimSpace(os.Getenv("USER"))
	}
	if cwd == "" || lineage == "" || expected == "" || decision == "" || reason == "" {
		err = errors.New("review decide requires --cwd, --lineage, --expected-revision, --decision, and --reason")
		return
	}
	if !validReviewCapabilitySHA256(expected) {
		err = errors.New("--expected-revision must be a sha256:hex authority identity")
		return
	}
	switch decision {
	case "continue", "stop":
	default:
		err = reviewtransaction.ErrInvalidDecisionFlag
		return
	}
	return
}

// RunReviewDecide resolves one decision_required lineage with the user's
// CAS-protected decision. The flow is:
//
//  1. Parse and validate flags (literal, expected-revision format).
//  2. Resolve the authoritative compact store for the lineage.
//  3. Reject non-DecisionRequired states with ErrIllegalStateForDecide.
//  4. Reject a stale --expected-revision with ErrAuthorityRevisionMismatch.
//  5. Reject a conflicting previously recorded decision with
//     ErrDecisionConflict.
//  6. If a same-decision payload already exists, return an idempotent
//     replay result without committing a new transition.
//  7. Otherwise, transition the lineage (edge 3 → Escalated, edge 4 →
//     DecisionCarryOn) and persist the DecisionPayload via the compact
//     store's authority-revision CAS.
//
// The function never invokes the bounded decision-adjudicate-batch
// consumer; that path is owned by RunReviewDecisionAdjudicate in WU5.
func RunReviewDecide(args []string, stdout io.Writer) error {
	return runReviewDecide(context.Background(), args, stdout)
}

func runReviewDecide(ctx context.Context, args []string, stdout io.Writer) error {
	cwd, lineage, expected, decision, reason, recordedBy, err := ParseReviewDecideFlags("review decide", stdout, args)
	if err != nil {
		return err
	}
	if reviewHelpRequested(args) {
		return nil
	}

	store, err := reviewtransaction.CompactAuthoritativeStore(ctx, cwd, lineage)
	if err != nil {
		return fmt.Errorf("resolve review decide store: %w", err)
	}
	record, err := store.LoadContext(ctx)
	if err != nil {
		return fmt.Errorf("load review decide lineage: %w", err)
	}
	if record.State.State != reviewtransaction.StateDecisionRequired {
		return fmt.Errorf("%w: got %q", reviewtransaction.ErrIllegalStateForDecide, record.State.State)
	}
	if record.Revision != expected {
		return fmt.Errorf("%w: expected %q, current %q", reviewtransaction.ErrAuthorityRevisionMismatch, expected, record.Revision)
	}
	if existing := record.State.Decision; existing != nil {
		if existing.Decision != decision {
			return fmt.Errorf("%w: recorded %q, requested %q", reviewtransaction.ErrDecisionConflict, existing.Decision, decision)
		}
		return encodeDecideResult(stdout, existing, reason, true, record.State.State, record.Revision)
	}

	next := record.State
	target := reviewtransaction.StateEscalated
	if decision == "continue" {
		target = reviewtransaction.StateDecisionCarryOn
	}
	next.State = target
	next.Decision = &reviewtransaction.DecisionPayload{
		Operation:  reviewtransaction.CompactDecideOperation,
		LineageID:  lineage,
		Decision:   decision,
		Revision:   record.Revision,
		RecordedBy: recordedBy,
		RecordedAt: time.Now().UTC(),
	}
	revision, err := store.ReplaceContext(ctx, record.Revision, reviewtransaction.CompactDecideOperation, next)
	if err != nil {
		return fmt.Errorf("commit review decide transition: %w", err)
	}
	if next.Decision == nil {
		return errors.New("review decide persisted without a payload")
	}
	return encodeDecideResult(stdout, next.Decision, reason, false, next.State, revision)
}

// encodeDecideResult emits the JSON envelope and, when the call runs under
// the negotiated review-integration contract, wraps it in the standard
// envelope. The function intentionally reuses the existing
// encodeReviewIntegrationOperation helper so the public envelope keeps the
// same field exclusions (no cwd, repository, or path leakage) as finalize
// and validate.
func encodeDecideResult(stdout io.Writer, payload *reviewtransaction.DecisionPayload, reason string, idempotent bool, state reviewtransaction.State, revision string) error {
	result := ReviewDecideResult{
		Schema:     ReviewDecideSchema,
		Operation:  reviewtransaction.CompactDecideOperation,
		LineageID:  payload.LineageID,
		Decision:   payload.Decision,
		Reason:     reason,
		State:      state,
		StoreRev:   revision,
		Idempotent: idempotent,
		RecordedBy: payload.RecordedBy,
		RecordedAt: payload.RecordedAt,
		Payload:    *payload,
	}
	return encodeReviewIntegrationOperation(stdout, false, reviewtransaction.CompactDecideOperation, result, ReviewIntegrationDecideResult{
		Operation:     reviewtransaction.CompactDecideOperation,
		LineageID:     payload.LineageID,
		Decision:      payload.Decision,
		State:         state,
		StoreRevision: revision,
		Idempotent:    idempotent,
	})
}
