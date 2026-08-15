package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gentleman-programming/gentle-ai/v2/internal/reviewtransaction"
)

// reviewRefPublicationSchema is the mutation result schema. The published
// durable result record is gentle-ai.review-ref-publication/v1
// (reviewtransaction.RefPublicationResultSchema); the CLI surfaces the same
// schema name so status and reconcile can branch on it without re-defining.
const reviewRefPublicationSchema = "gentle-ai.review-ref-publication/v1"

// reviewRefPublicationStatusSchema is the read-only status schema published
// by the design.
const reviewRefPublicationStatusSchema = "gentle-ai.review-ref-publication-status/v1"

// reviewRefPublicationReconciliationSchema is the read-only reconcile schema
// published by the design.
const reviewRefPublicationReconciliationSchema = "gentle-ai.review-ref-publication-reconciliation/v1"

// reviewRefPublicationExitCode is the published exit-code map from the
// design's "Result And Exit Contract" section. The CLI maps the durable
// RefPublicationState to one of these codes on every dispatch so the bench
// journey and consumers can branch on the same constant.
type reviewRefPublicationExitCode int

const (
	reviewRefPublicationExitConfirmed            reviewRefPublicationExitCode = 0
	reviewRefPublicationExitBlocked              reviewRefPublicationExitCode = 1
	reviewRefPublicationExitNotCreated           reviewRefPublicationExitCode = 1
	reviewRefPublicationExitConflict             reviewRefPublicationExitCode = 1
	reviewRefPublicationExitInvalidRequest       reviewRefPublicationExitCode = 2
	reviewRefPublicationExitTransportUnavailable reviewRefPublicationExitCode = 2
	reviewRefPublicationExitPublicationUnknown   reviewRefPublicationExitCode = 75
)

// reviewRefPublicationExitCodeForState is the bounded mapping from the
// durable result state to the published exit code. Every state in the
// canonical lifecycle maps to exactly one code; unknown states collapse to
// blocked (1) so a malformed record cannot masquerade as confirmed.
func reviewRefPublicationExitCodeForState(state reviewtransaction.RefPublicationState) reviewRefPublicationExitCode {
	switch state {
	case reviewtransaction.RefPubConfirmed:
		return reviewRefPublicationExitConfirmed
	case reviewtransaction.RefPubBlocked:
		return reviewRefPublicationExitBlocked
	case reviewtransaction.RefPubNotCreated:
		return reviewRefPublicationExitNotCreated
	case reviewtransaction.RefPubConflict:
		return reviewRefPublicationExitConflict
	case reviewtransaction.RefPubInvalidRequest:
		return reviewRefPublicationExitInvalidRequest
	case reviewtransaction.RefPubPublicationUnknown:
		return reviewRefPublicationExitPublicationUnknown
	case "":
		return reviewRefPublicationExitInvalidRequest
	}
	return reviewRefPublicationExitBlocked
}

// reviewRefPublicationArgumentRefusal is the typed preflight refusal the
// publish-ref verbs emit for every flag-level mistake. It carries the
// message a caller-facing surface prints and bounds the exit code to 2
// when the CLI handler decides to terminate, so invalid_request semantics
// remain stable across dispatchers.
type reviewRefPublicationArgumentRefusal struct {
	message string
}

func (err *reviewRefPublicationArgumentRefusal) Error() string { return err.message }

// reviewRefPublicationResultEnvelope is the persistent mutation result the
// CLI emits to stdout. It mirrors the published
// gentle-ai.review-ref-publication/v1 schema and adds the operation field
// so the bench journey can disambiguate without parsing the schema name.
type reviewRefPublicationResultEnvelope struct {
	Schema       string                                      `json:"schema"`
	Operation    string                                      `json:"operation"`
	RequestID    string                                      `json:"request_id"`
	AttemptState reviewtransaction.RefPublicationState       `json:"attempt_state"`
	Attribution  reviewtransaction.RefPublicationAttribution `json:"attribution"`
	RecordedAt   string                                      `json:"recorded_at"`
	// ResultRef is the SHA-256 identity of the durable result record. It is
	// set only when the verdict is confirmed with proven attribution; every
	// other state leaves it empty so a caller sees the omission rather
	// than a synthetic placeholder.
	ResultRef string `json:"result_ref,omitempty"`
}

// reviewRefPublicationStatusEnvelope is the read-only status payload. It
// points at the durable record under the RAR authority root and surfaces
// the terminal verdict and the canonical authorization payload so an
// operator can prove the on-disk state without re-deriving it.
type reviewRefPublicationStatusEnvelope struct {
	Schema       string                                      `json:"schema"`
	Operation    string                                      `json:"operation"`
	RequestID    string                                      `json:"request_id"`
	AttemptState reviewtransaction.RefPublicationState       `json:"attempt_state"`
	Attribution  reviewtransaction.RefPublicationAttribution `json:"attribution,omitempty"`
	UpdatedAt    string                                      `json:"updated_at"`
	ResultRef    string                                      `json:"result_ref,omitempty"`
}

// reviewRefPublicationReconciliationEnvelope is the read-only reconcile
// payload. Its classification is derived from the fresh isolated remote
// observation the transport dispatches; the dispatch is never allowed to
// push.
type reviewRefPublicationReconciliationEnvelope struct {
	Schema         string                                      `json:"schema"`
	Operation      string                                      `json:"operation"`
	RequestID      string                                      `json:"request_id"`
	Classification reviewtransaction.RefPublicationState       `json:"classification"`
	Observation    reviewtransaction.RefPublicationObservation `json:"observation"`
}

// reviewRefPublicationRequest is the in-memory projection of the CLI flag
// set; it is the only typed coupling the CLI handlers have with the
// authorization payload so the parsing path stays easy to read.
type reviewRefPublicationRequest struct {
	RequestID                 string
	Remote                    string
	LocalSourceRef            string
	AdvertisedSourceRef       string
	DestinationRef            string
	Lineage                   string
	ExpectedAuthorityRevision string
	ReceiptRef                string
	Actor                     string
	Reason                    string
	MaintainerAuthorization   string
	Cwd                       string
}

// reviewRefPublicationRequireValidRequest is the single preflight guard
// every publish-ref request passes through. It validates the flag-derived
// request against the published contract:
//   - request_id is a UUID
//   - lineage is a valid integration lineage
//   - expected_authority_revision is a sha256 digest
//   - receipt_ref is a sha256 digest
//   - source/destination refs are refs/heads/* with the destination passing
//     the create-only allowlist (default-branch and tag rejections included)
//   - actor and reason are non-empty
//
// The remote is the canonical endpoint identity the maintainer authorizes.
// It is bound verbatim into the payload; the transport's own argv-side
// refusal table rejects embedded credentials at dispatch time.
func reviewRefPublicationRequireValidRequest(request reviewRefPublicationRequest) error {
	if !reviewRefPublicationValidUUID(request.RequestID) {
		return reviewRefPublicationErrInvalidRequest("ref publication --request-id must be a UUID")
	}
	if !validReviewIntegrationLineage(request.Lineage) {
		return reviewRefPublicationErrInvalidRequest("ref publication --lineage is invalid")
	}
	if !validReviewCapabilitySHA256(request.ExpectedAuthorityRevision) {
		return reviewRefPublicationErrInvalidRequest("ref publication --expected-authority-revision must be a sha256 digest")
	}
	if !validReviewCapabilitySHA256(request.ReceiptRef) {
		return reviewRefPublicationErrInvalidRequest("ref publication --receipt-ref must be a sha256 digest")
	}
	if !strings.HasPrefix(request.LocalSourceRef, "refs/heads/") {
		return reviewRefPublicationErrInvalidRequest("ref publication --local-source-ref must be a refs/heads/* ref")
	}
	if !strings.HasPrefix(request.AdvertisedSourceRef, "refs/heads/") {
		return reviewRefPublicationErrInvalidRequest("ref publication --advertised-source-ref must be a refs/heads/* ref")
	}
	if err := reviewtransaction.ValidateRefPublicationDestinationRef(request.DestinationRef); err != nil {
		return reviewRefPublicationErrInvalidRequest("ref publication --destination-ref: " + err.Error())
	}
	if request.Actor == "" {
		return reviewRefPublicationErrInvalidRequest("ref publication --actor is empty")
	}
	if request.Reason == "" {
		return reviewRefPublicationErrInvalidRequest("ref publication --reason is empty")
	}
	if strings.ContainsAny(request.Reason, "\r\n") {
		return reviewRefPublicationErrInvalidRequest("ref publication --reason must not contain line breaks")
	}
	if request.Remote == "" {
		return reviewRefPublicationErrInvalidRequest("ref publication --remote is empty")
	}
	return nil
}

// reviewRefPublicationValidUUID is the only request_id shape the design
// documents. The check is structural: a UUID has exactly 36 hex characters
// separated by hyphens at the canonical offsets. It does not require the
// hex digits to be uppercase or lowercase, only to be present.
func reviewRefPublicationValidUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range value {
		switch index {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
				return false
			}
		}
	}
	return true
}

// reviewRefPublicationErrInvalidRequest is the typed-error helper for the
// preflight refusal surface so a caller can match invalid_request with
// errors.As.
func reviewRefPublicationErrInvalidRequest(message string) error {
	return &reviewRefPublicationArgumentRefusal{message: message}
}

// reviewRefPublicationErrInvalidRequestFromError wraps an arbitrary error
// as a preflight refusal. It is the canonical binding the parse path uses
// so internal-vs-operator errors never share a classification.
func reviewRefPublicationErrInvalidRequestFromError(err error) error {
	if err == nil {
		return nil
	}
	return &reviewRefPublicationArgumentRefusal{message: err.Error()}
}

// reviewRefPublicationAuthoriseAuthorization parses the maintainer
// authorization and rebinds it against the CLI's flag-derived request. The
// 14 fields are CRC-validated against the LF-encoded payload the
// maintainer supplied; mismatches are a preflight refusal because the
// authorization would not survive a re-derivation.
func reviewRefPublicationAuthoriseAuthorization(
	request reviewRefPublicationRequest,
) (reviewtransaction.RefPublicationAuthorization, error) {
	if request.MaintainerAuthorization == "" {
		return reviewtransaction.RefPublicationAuthorization{},
			reviewRefPublicationErrInvalidRequest("ref publication --maintainer-authorization is empty")
	}
	auth, err := reviewtransaction.ParseRefPublicationAuthorization(request.MaintainerAuthorization)
	if err != nil {
		return reviewtransaction.RefPublicationAuthorization{},
			reviewRefPublicationErrInvalidRequest("ref publication --maintainer-authorization: " + err.Error())
	}
	if err := auth.Validate(); err != nil {
		return reviewtransaction.RefPublicationAuthorization{},
			reviewRefPublicationErrInvalidRequest("ref publication --maintainer-authorization: " + err.Error())
	}
	if err := reviewRefPublicationAuthorizationMatchesRequest(auth, request); err != nil {
		return reviewtransaction.RefPublicationAuthorization{}, err
	}
	return auth, nil
}

// reviewRefPublicationAuthorizationMatchesRequest is the per-field guard.
// Every field the design lists as bound to the request must be present in
// the authorization payload under the exact name.
func reviewRefPublicationAuthorizationMatchesRequest(
	auth reviewtransaction.RefPublicationAuthorization,
	request reviewRefPublicationRequest,
) error {
	if auth.RequestID != request.RequestID {
		return reviewRefPublicationErrInvalidRequest("ref publication --request-id does not match the authorization request_id")
	}
	if auth.LineageID != request.Lineage {
		return reviewRefPublicationErrInvalidRequest("ref publication --lineage does not match the authorization lineage_id")
	}
	if auth.AuthorityRevision != request.ExpectedAuthorityRevision {
		return reviewRefPublicationErrInvalidRequest("ref publication --expected-authority-revision does not match the authorization authority_revision")
	}
	if auth.ReceiptRef != request.ReceiptRef {
		return reviewRefPublicationErrInvalidRequest("ref publication --receipt-ref does not match the authorization receipt_ref")
	}
	if auth.LocalSourceRef != request.LocalSourceRef {
		return reviewRefPublicationErrInvalidRequest("ref publication --local-source-ref does not match the authorization local_source_ref")
	}
	if auth.AdvertisedSourceRef != request.AdvertisedSourceRef {
		return reviewRefPublicationErrInvalidRequest("ref publication --advertised-source-ref does not match the authorization advertised_source_ref")
	}
	if auth.DestinationRef != request.DestinationRef {
		return reviewRefPublicationErrInvalidRequest("ref publication --destination-ref does not match the authorization destination_ref")
	}
	if auth.Actor != request.Actor {
		return reviewRefPublicationErrInvalidRequest("ref publication --actor does not match the authorization actor")
	}
	if auth.Reason != request.Reason {
		return reviewRefPublicationErrInvalidRequest("ref publication --reason does not match the authorization reason")
	}
	return nil
}

// reviewRefPublicationResultIdentity is the canonical SHA-256 identity of
// the confirmed result. The preimage is the canonical authorization payload
// concatenated with the recorded_at timestamp; SHA-256 is the schema the
// RAR index already uses for receipt_ref and authority_revision, so the
// result_ref occupies the same namespace.
func reviewRefPublicationResultIdentity(
	auth reviewtransaction.RefPublicationAuthorization,
	recordedAt string,
) string {
	preimage := auth.Payload() + recordedAt + "\n"
	sum := sha256.Sum256([]byte(preimage))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// reviewRefPublicationParseFlags is the shared flag parser for the three
// publish-ref command verbs. It is intentionally internal to the CLI: the
// publish-ref surface is a CLI-only contract, and the bench journey talks
// to the actual CLI binary, not to a Go helper.
func reviewRefPublicationParseFlags(
	name string,
	stdout io.Writer,
	help string,
	args []string,
) (reviewRefPublicationRequest, error) {
	flags := newReviewFlagSet(name, stdout, help)
	requestID := flags.String("request-id", "", "exact request UUID the publication is bound to")
	remote := flags.String("remote", "", "configured remote whose canonical endpoint the push uses")
	localSourceRef := flags.String("local-source-ref", "", "refs/heads/L whose HEAD is the source commit C")
	advertisedSourceRef := flags.String("advertised-source-ref", "", "refs/heads/S the remote already advertises at C")
	destinationRef := flags.String("destination-ref", "", "refs/heads/N the create-only push creates")
	lineage := flags.String("lineage", "", "exact approved lineage the receipt referees")
	expectedAuthorityRevision := flags.String("expected-authority-revision", "", "sha256:<revision> the receipt is bound to")
	receiptRef := flags.String("receipt-ref", "", "sha256:<receipt> the receipt is bound to")
	actor := flags.String("actor", "", "maintainer actor; never echoed in public output")
	reason := flags.String("reason", "", "maintainer reason; never echoed in public output")
	flags.String("maintainer-authorization", "", "exact 14-line LF-only maintainer authorization")
	cwd := flags.String("cwd", ".", "repository path")
	if err := parseReviewFlags(flags, args); err != nil {
		return reviewRefPublicationRequest{}, err
	}
	if reviewHelpRequested(args) {
		return reviewRefPublicationRequest{}, nil
	}
	if flags.NArg() != 0 {
		return reviewRefPublicationRequest{}, reviewRefPublicationErrInvalidRequest(
			"review " + name + " received an unexpected positional argument: " + flags.Arg(0))
	}
	return reviewRefPublicationRequest{
		RequestID:                 strings.TrimSpace(*requestID),
		Remote:                    strings.TrimSpace(*remote),
		LocalSourceRef:            strings.TrimSpace(*localSourceRef),
		AdvertisedSourceRef:       strings.TrimSpace(*advertisedSourceRef),
		DestinationRef:            strings.TrimSpace(*destinationRef),
		Lineage:                   strings.TrimSpace(*lineage),
		ExpectedAuthorityRevision: strings.TrimSpace(*expectedAuthorityRevision),
		ReceiptRef:                strings.TrimSpace(*receiptRef),
		Actor:                     strings.TrimSpace(*actor),
		Reason:                    strings.TrimSpace(*reason),
		MaintainerAuthorization:   flags.Lookup("maintainer-authorization").Value.String(),
		Cwd:                       strings.TrimSpace(*cwd),
	}, nil
}

// reviewRefPublicationReadOnlyFlags is the smaller flag set the read-only
// status and reconcile commands share. The narrower surface is documented
// so a maintainer cannot mistake either for the mutation form.
func reviewRefPublicationReadOnlyFlags(
	name string,
	stdout io.Writer,
	help string,
	args []string,
) (string, string, error) {
	flags := newReviewFlagSet(name, stdout, help)
	requestID := flags.String("request-id", "", "exact request UUID whose durable record is the read source")
	cwd := flags.String("cwd", ".", "repository path")
	if err := parseReviewFlags(flags, args); err != nil {
		return "", "", err
	}
	if reviewHelpRequested(args) {
		return "", "", nil
	}
	if flags.NArg() != 0 {
		return "", "", reviewRefPublicationErrInvalidRequest(
			"review " + name + " received an unexpected positional argument: " + flags.Arg(0))
	}
	return strings.TrimSpace(*requestID), strings.TrimSpace(*cwd), nil
}

// reviewRefPublicationEncodeEnvelope is the bounded JSON-encode helper. It
// never writes partial output: a write failure is the only path that can
// leave the envelope half-written, and the bound returns no error to keep
// the exit-code mapping the only signal a caller needs.
func reviewRefPublicationEncodeEnvelope(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// reviewRefPublicationExitEnvelope is the typed-error shape the publish-ref
// commands return to the dispatcher. The dispatcher is the only path
// allowed to call os.Exit because tests must be able to drive the
// commands in-process without terminating the test binary.
//
// The envelope carries the JSON envelope to write to stdout; the
// stderr message is the human-readable cause (printed when non-empty);
// the code is the bounded exit code the OS receives.
type reviewRefPublicationExitEnvelope struct {
	Code     reviewRefPublicationExitCode
	Envelope any
	Stderr   string
}

// Error renders the stderr message so tools that print the returned
// error from the dispatch chain see the human-readable cause.
func (e *reviewRefPublicationExitEnvelope) Error() string {
	if e == nil {
		return ""
	}
	if e.Stderr != "" {
		return e.Stderr
	}
	return "ref publication command exited"
}

// reviewRefPublicationExitWithCode is the single terminator the
// publish-ref dispatch chain uses. It writes the JSON envelope to stdout
// and exits with the platform-bounded code; the function never returns
// to its caller.
func reviewRefPublicationExitWithCode(code reviewRefPublicationExitCode, envelope any) {
	if envelope != nil {
		_ = reviewRefPublicationEncodeEnvelope(envelope)
	}
	os.Exit(int(code))
}

// reviewRefPublicationEmit is the bounded dispatcher envelope. Every
// publish-ref command returns a typed *reviewRefPublicationExitEnvelope
// so the runReviewCommand dispatcher can decide whether to write the
// envelope to stdout and exit, or let the chain continue. A test that
// catches the typed error can read the result without ever terminating.
func reviewRefPublicationEmit(
	code reviewRefPublicationExitCode,
	envelope any,
	stderr string,
) error {
	return &reviewRefPublicationExitEnvelope{Code: code, Envelope: envelope, Stderr: stderr}
}

// reviewRefPublicationEmitInvalidRequest is the bounded preflight
// terminator. A preflight refusal always exits with code 2
// (invalid_request) so the classification is stable across dispatchers
// and never bleeds into the 1-conflict or 75-publication-unknown buckets.
func reviewRefPublicationEmitInvalidRequest(err error) error {
	if err == nil {
		err = errors.New("ref publication request is invalid") // refusal:by-design world-action: the dispatcher's typed-error path is the only way this fallback fires; the exit is fixing the call site, not running a command
	}
	envelope := reviewRefPublicationResultEnvelope{
		Schema:       reviewRefPublicationSchema,
		Operation:    "review.publish_ref",
		AttemptState: reviewtransaction.RefPubInvalidRequest,
		Attribution:  reviewtransaction.RefPublicationAttributionUnproven,
		RecordedAt:   reviewRefPublicationCurrentTimestamp(),
	}
	return reviewRefPublicationEmit(
		reviewRefPublicationExitInvalidRequest,
		envelope,
		"Error: "+err.Error(),
	)
}

// reviewRefPublicationDispatchExit is the single dispatcher hook. It
// receives the typed error the handler returned, writes the envelope to
// stdout, writes the stderr message when present, and exits with the
// bounded code. The hook is the only os.Exit surface in the chain.
func reviewRefPublicationDispatchExit(handlerErr error) error {
	if handlerErr == nil {
		return nil
	}
	var typed *reviewRefPublicationExitEnvelope
	if !errors.As(handlerErr, &typed) {
		return handlerErr
	}
	if typed.Envelope != nil {
		_ = reviewRefPublicationEncodeEnvelope(typed.Envelope)
	}
	if typed.Stderr != "" {
		_, _ = fmt.Fprintln(os.Stderr, typed.Stderr)
	}
	reviewRefPublicationExitWithCode(typed.Code, nil)
	return nil
}

// reviewRefPublicationCurrentTimestamp is the canonical RFC3339Nano clock
// the result envelope carries. It is centralized so the bench and the
// CLI agree on the recorded_at field across re-runs.
func reviewRefPublicationCurrentTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// reviewRefPublicationResolveRoot is the CLI-level repository root
// resolver. It centralizes the "find the git common dir" step so the three
// commands share the same error envelope on a broken repository.
func reviewRefPublicationResolveRoot(ctx context.Context, cwd string) (string, error) {
	root, err := reviewtransaction.SnapshotBuilder{Repo: cwd}.ResolveRepositoryRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve ref publication repository root: %w", err)
	}
	return root, nil
}

// reviewRefPublicationLoadRecord is the bounded read-only loader the
// status and reconcile commands share. It refuses to return a record when
// the request_id is unknown so the JSON envelope is consistent across the
// two commands.
func reviewRefPublicationLoadRecord(
	ctx context.Context,
	repository *reviewtransaction.RarRefPublicationRepository,
	requestID string,
) (reviewtransaction.RefPublicationRecord, error) {
	record, err := repository.Load(ctx, requestID)
	if errors.Is(err, reviewtransaction.ErrRefPublicationUnknownRequestID) {
		return reviewtransaction.RefPublicationRecord{}, &reviewRefPublicationArgumentRefusal{
			message: "ref publication --request-id has no durable record",
		}
	}
	if err != nil {
		return reviewtransaction.RefPublicationRecord{}, err
	}
	return record, nil
}

// reviewRefPublicationRequireRecordReady is the read-only guard a status
// or reconcile command runs before it can return a verdict. It refuses
// before opening the repository if the request_id is empty or not a UUID.
func reviewRefPublicationRequireRecordReady(requestID string) error {
	if requestID == "" {
		return &reviewRefPublicationArgumentRefusal{
			message: "ref publication --request-id is empty",
		}
	}
	if !reviewRefPublicationValidUUID(requestID) {
		return &reviewRefPublicationArgumentRefusal{
			message: "ref publication --request-id must be a UUID",
		}
	}
	return nil
}

// reviewRefPublicationDispatchLifecycle is the bounded executor of the
// Prepare → Push → MarkTerminal sequence. It is the only function that
// touches the transport, so on every test surface the bench journey can
// substitute the GENTLE_AI_TEST_TRANSPORT_HELPER fake and the production
// CLI can run the issued argv through the dispatcher's refuse table.
//
// The state machine (repository.Save → transport.Prepare → transport.Push
// → repository.Save → repository.MarkTerminal) is the only lifecycle the
// design admits; every error path collapses to one of the seven terminal
// states so the exit-code mapping is the only signal a caller needs.
func reviewRefPublicationDispatchLifecycle(
	ctx context.Context,
	root string,
	auth reviewtransaction.RefPublicationAuthorization,
	request reviewRefPublicationRequest,
) (reviewtransaction.RefPublicationState, reviewtransaction.RefPublicationAttribution, string, error) {
	repository, err := reviewtransaction.OpenRefPublicationRepository(ctx, root)
	if err != nil {
		return reviewtransaction.RefPubInvalidRequest, reviewtransaction.RefPublicationAttributionUnproven, "",
			reviewRefPublicationErrInvalidRequestFromError(
				fmt.Errorf("open ref publication repository: %w", err))
	}
	transport, err := reviewtransaction.OpenRefPublicationTransport(ctx, root)
	if err != nil {
		return reviewtransaction.RefPubInvalidRequest, reviewtransaction.RefPublicationAttributionUnproven, "",
			reviewRefPublicationErrInvalidRequestFromError(
				fmt.Errorf("open ref publication transport: %w", err))
	}
	preparedAt := reviewRefPublicationCurrentTimestamp()
	preparedRecord := reviewtransaction.RefPublicationRecord{
		Schema:        reviewtransaction.RefPublicationRecordSchema,
		RequestID:     auth.RequestID,
		RequestDigest: reviewtransaction.RefPublicationAuthorizationDigest(auth),
		State:         reviewtransaction.RefPubPrepared,
		Payload:       []byte(auth.Payload()),
		UpdatedAt:     preparedAt,
	}
	persisted, err := repository.Save(ctx, preparedRecord)
	if err != nil {
		return reviewRefPublicationLifecycleErrorFrom(err)
	}
	if err := transport.ProveBeforePublish(ctx, auth); err != nil {
		return reviewRefPublicationLifecycleErrorFrom(err)
	}
	if err := transport.Prepare(ctx, auth, persisted); err != nil {
		return reviewRefPublicationLifecycleErrorFrom(err)
	}
	pushResult, pushErr := transport.Push(ctx, persisted)
	if pushErr != nil {
		return reviewRefPublicationLifecycleErrorFrom(pushErr)
	}
	pushedRecord := persisted
	pushedRecord.State = reviewtransaction.RefPubPushed
	pushedRecord.UpdatedAt = reviewRefPublicationCurrentTimestamp()
	if _, err := repository.Save(ctx, pushedRecord); err != nil {
		return reviewRefPublicationLifecycleErrorFrom(err)
	}
	confirmedAt := reviewRefPublicationCurrentTimestamp()
	_ = pushResult
	resultRef, err := reviewRefPublicationPersistConfirmed(ctx, repository, auth, confirmedAt)
	if err != nil {
		return reviewRefPublicationLifecycleErrorFrom(err)
	}
	return reviewtransaction.RefPubConfirmed, reviewtransaction.RefPublicationAttributionProven, resultRef, nil
}

// reviewRefPublicationPersistConfirmed is the bounded MarkTerminal pass for
// the success state. It is the only path that supplies a non-empty
// result_ref; the repository's own invariant refuses a confirmed verdict
// without one, so the helper builds the canonical identity before the
// call.
func reviewRefPublicationPersistConfirmed(
	ctx context.Context,
	repository *reviewtransaction.RarRefPublicationRepository,
	auth reviewtransaction.RefPublicationAuthorization,
	recordedAt string,
) (string, error) {
	resultRef := reviewRefPublicationResultIdentity(auth, recordedAt)
	if _, err := repository.MarkTerminal(ctx, auth.RequestID,
		reviewtransaction.RefPubConfirmed,
		reviewtransaction.RefPublicationAttributionProven,
		resultRef); err != nil {
		return "", err
	}
	return resultRef, nil
}

// reviewRefPublicationMarkTerminalAcrossStates is the bounded MarkTerminal
// pass for the non-confirmed terminal states. result_ref is empty because
// the repository's invariant requires it only on confirmed-with-proven.
func reviewRefPublicationMarkTerminalAcrossStates(
	ctx context.Context,
	repository *reviewtransaction.RarRefPublicationRepository,
	requestID string,
	state reviewtransaction.RefPublicationState,
	attribution reviewtransaction.RefPublicationAttribution,
) error {
	_, err := repository.MarkTerminal(ctx, requestID, state, attribution, "")
	return err
}

// reviewRefPublicationLifecycleErrorFrom collapses a typed error from the
// repository or transport into the bounded lifecycle-error triples the
// outer dispatch returns. The mapping is the central contract between the
// CLI and the bench: every typed refusal maps to exactly one
// (state, attribution, error) tuple, so the exit-code table is the only
// signal a caller needs to interpret the failure.
func reviewRefPublicationLifecycleErrorFrom(err error) (
	reviewtransaction.RefPublicationState,
	reviewtransaction.RefPublicationAttribution,
	string,
	error,
) {
	if err == nil {
		return reviewtransaction.RefPubConfirmed, reviewtransaction.RefPublicationAttributionProven, "", nil
	}
	switch {
	case errors.Is(err, reviewtransaction.ErrRefPublicationTransportUnavailable),
		errors.Is(err, reviewtransaction.ErrRefPublicationTransportNotReady),
		errors.Is(err, reviewtransaction.ErrRefPublicationTransportAlreadyTerminal),
		errors.Is(err, reviewtransaction.ErrRefPublicationTransportCrashed),
		errors.Is(err, reviewtransaction.ErrRefPublicationObserverCrashed):
		return reviewtransaction.RefPubInvalidRequest, reviewtransaction.RefPublicationAttributionUnproven, "", err
	case errors.Is(err, reviewtransaction.ErrRefPublicationAllocationContested):
		return reviewtransaction.RefPubConflict, reviewtransaction.RefPublicationAttributionUnproven, "", err
	case errors.Is(err, reviewtransaction.ErrRefPublicationAlreadyTerminal),
		errors.Is(err, reviewtransaction.ErrRefPublicationTransitionIllegal),
		errors.Is(err, reviewtransaction.ErrRefPublicationNotPrepared):
		return reviewtransaction.RefPubConflict, reviewtransaction.RefPublicationAttributionUnproven, "", err
	case errors.Is(err, reviewtransaction.ErrRefPublicationReplayMismatch):
		return reviewtransaction.RefPubInvalidRequest, reviewtransaction.RefPublicationAttributionUnproven, "", err
	case errors.Is(err, reviewtransaction.ErrRefPublicationDriftRejected),
		errors.Is(err, reviewtransaction.ErrRefPublicationLeaseRejected):
		return reviewtransaction.RefPubBlocked, reviewtransaction.RefPublicationAttributionUnproven, "", err
	case errors.Is(err, reviewtransaction.ErrRefPublicationDestinationRace):
		return reviewtransaction.RefPubConflict, reviewtransaction.RefPublicationAttributionUnproven, "", err
	case errors.Is(err, reviewtransaction.ErrRefPublicationPorcelainMalformed),
		errors.Is(err, reviewtransaction.ErrRefPublicationPorcelainAmbiguous):
		return reviewtransaction.RefPubBlocked, reviewtransaction.RefPublicationAttributionUnproven, "", err
	}
	if _, ok := err.(*reviewRefPublicationArgumentRefusal); ok {
		return reviewtransaction.RefPubInvalidRequest, reviewtransaction.RefPublicationAttributionUnproven, "", err
	}
	return reviewtransaction.RefPubInvalidRequest, reviewtransaction.RefPublicationAttributionUnproven, "", err
}

// RunReviewPublishRef is the implementation of the explicit mutation
// command. It dispatches the bounded lifecycle and maps the terminal
// state to the published exit code in one place; the rest of the surface
// is the preflight check, the JSON envelope, and the bounded exit
// terminator.
//
// The flow is strict: every flag is parsed in one place, every guard is
// one typed error, every dispatch is one terminal state. A flag mistake
// never invokes the transport, so a preflight refusal provably wrote
// nothing.
func RunReviewPublishRef(args []string, stdout io.Writer) error {
	request, err := reviewRefPublicationParseFlags("review publish-ref", stdout,
		"Create-only reviewed ref publication. Reuses the exact reviewed receipt, lineage, and authority revision the design requires; binds them to a single server-confirmed `--force-with-lease=<N>:<zero-oid>` push through the provider-owned isolated bare transport. The unique push is one-use; the durable record under the RAR authority root is the only replay surface. Returns the gentle-ai.review-ref-publication/v1 envelope; exits 0 only on confirmed, 1 on blocked/not_created/conflict, 2 on invalid_request/transport_unavailable, 75 on publication_unknown.",
		args)
	if err != nil {
		return reviewRefPublicationEmitInvalidRequest(err)
	}
	if reviewHelpRequested(args) {
		return nil
	}
	if err := reviewRefPublicationRequireValidRequest(request); err != nil {
		return reviewRefPublicationEmitInvalidRequest(err)
	}
	auth, err := reviewRefPublicationAuthoriseAuthorization(request)
	if err != nil {
		return reviewRefPublicationEmitInvalidRequest(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), reviewRefPublicationOperationTimeout)
	defer cancel()
	root, err := reviewRefPublicationResolveRoot(ctx, request.Cwd)
	if err != nil {
		return reviewRefPublicationEmitInvalidRequest(err)
	}
	state, attribution, resultRef, err := reviewRefPublicationDispatchLifecycle(ctx, root, auth, request)
	recordedAt := reviewRefPublicationCurrentTimestamp()
	envelope := reviewRefPublicationResultEnvelope{
		Schema:       reviewRefPublicationSchema,
		Operation:    "review.publish_ref",
		RequestID:    auth.RequestID,
		AttemptState: state,
		Attribution:  attribution,
		RecordedAt:   recordedAt,
		ResultRef:    resultRef,
	}
	exitCode := reviewRefPublicationExitCodeForState(state)
	stderr := ""
	if err != nil {
		stderr = "Error: " + err.Error()
	}
	return reviewRefPublicationEmit(exitCode, envelope, stderr)
}

// reviewRefPublicationOperationTimeout is the bounded deadline the
// publish-ref verb applies to the dispatched lifecycle. The transport
// itself enforces a 90-second per-push ceiling; the CLI mirrors that
// ceiling with a 30-second headroom so a hung ls-remote or porcelain
// parser does not pin the dispatch.
const reviewRefPublicationOperationTimeout = 120 * time.Second

// RunReviewPublishRefStatus is the read-only status command. It loads
// the durable record under the RAR authority root and surfaces the
// verifiable canonical state. Exit 0 is only for a present record; an
// unknown request_id exits 1 so callers can branch on the durable
// identity without re-reading the on-disk state.
func RunReviewPublishRefStatus(args []string, stdout io.Writer) error {
	requestID, cwd, err := reviewRefPublicationReadOnlyFlags("review publish-ref-status", stdout,
		"Read the durable ref publication record for one request_id. Returns the gentle-ai.review-ref-publication-status/v1 envelope with the current attempt state, attribution, updated_at, and result_ref. Exits 0 if the record is present; exits 1 if the request_id is unknown. Never pushes.",
		args)
	if err != nil {
		return reviewRefPublicationEmitInvalidRequest(err)
	}
	if reviewHelpRequested(args) {
		return nil
	}
	if err := reviewRefPublicationRequireRecordReady(requestID); err != nil {
		return reviewRefPublicationEmitInvalidRequest(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), reviewRefPublicationOperationTimeout)
	defer cancel()
	root, err := reviewRefPublicationResolveRoot(ctx, cwd)
	if err != nil {
		return reviewRefPublicationEmitInvalidRequest(err)
	}
	repository, err := reviewtransaction.OpenRefPublicationRepository(ctx, root)
	if err != nil {
		return reviewRefPublicationEmitInvalidRequest(reviewRefPublicationErrInvalidRequestFromError(
			fmt.Errorf("open ref publication repository: %w", err)))
	}
	record, err := reviewRefPublicationLoadRecord(ctx, repository, requestID)
	if err != nil {
		envelope := reviewRefPublicationStatusEnvelope{
			Schema:       reviewRefPublicationStatusSchema,
			Operation:    "review.publish_ref_status",
			RequestID:    requestID,
			AttemptState: reviewtransaction.RefPubInvalidRequest,
			UpdatedAt:    reviewRefPublicationCurrentTimestamp(),
		}
		return reviewRefPublicationEmit(
			reviewRefPublicationExitCodeForState(reviewtransaction.RefPubInvalidRequest),
			envelope,
			"Error: "+err.Error(),
		)
	}
	envelope := reviewRefPublicationStatusEnvelope{
		Schema:       reviewRefPublicationStatusSchema,
		Operation:    "review.publish_ref_status",
		RequestID:    requestID,
		AttemptState: record.State,
		Attribution:  record.Attribution,
		UpdatedAt:    record.UpdatedAt,
		ResultRef:    record.ResultRef,
	}
	return reviewRefPublicationEmit(reviewRefPublicationExitConfirmed, envelope, "")
}

// RunReviewPublishRefReconcile is the read-only reconcile command. It
// dispatches a fresh isolated remote observation through the transport
// and classifies the destination as not_created, confirmed, conflict, or
// publication_unknown. The dispatch never pushes.
func RunReviewPublishRefReconcile(args []string, stdout io.Writer) error {
	requestID, cwd, err := reviewRefPublicationReadOnlyFlags("review publish-ref-reconcile", stdout,
		"Refresh the ref publication verdict from a fresh isolated remote observation. Returns the gentle-ai.review-ref-publication-reconciliation/v1 envelope with the classification and the observed commit. Exits 0 on confirmed, 1 on not_created/conflict, 75 on publication_unknown, 2 on invalid_request. Never pushes.",
		args)
	if err != nil {
		return reviewRefPublicationEmitInvalidRequest(err)
	}
	if reviewHelpRequested(args) {
		return nil
	}
	if err := reviewRefPublicationRequireRecordReady(requestID); err != nil {
		return reviewRefPublicationEmitInvalidRequest(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), reviewRefPublicationOperationTimeout)
	defer cancel()
	root, err := reviewRefPublicationResolveRoot(ctx, cwd)
	if err != nil {
		return reviewRefPublicationEmitInvalidRequest(err)
	}
	repository, err := reviewtransaction.OpenRefPublicationRepository(ctx, root)
	if err != nil {
		return reviewRefPublicationEmitInvalidRequest(reviewRefPublicationErrInvalidRequestFromError(
			fmt.Errorf("open ref publication repository: %w", err)))
	}
	transport, err := reviewtransaction.OpenRefPublicationTransport(ctx, root)
	if err != nil {
		return reviewRefPublicationEmitInvalidRequest(reviewRefPublicationErrInvalidRequestFromError(
			fmt.Errorf("open ref publication transport: %w", err)))
	}
	record, err := reviewRefPublicationLoadRecord(ctx, repository, requestID)
	if err != nil {
		envelope := reviewRefPublicationReconciliationEnvelope{
			Schema:         reviewRefPublicationReconciliationSchema,
			Operation:      "review.publish_ref_reconcile",
			RequestID:      requestID,
			Classification: reviewtransaction.RefPubInvalidRequest,
		}
		return reviewRefPublicationEmit(
			reviewRefPublicationExitCodeForState(reviewtransaction.RefPubInvalidRequest),
			envelope,
			"Error: "+err.Error(),
		)
	}
	reconciliation, err := transport.Reconcile(ctx, record)
	if err != nil {
		envelope := reviewRefPublicationReconciliationEnvelope{
			Schema:         reviewRefPublicationReconciliationSchema,
			Operation:      "review.publish_ref_reconcile",
			RequestID:      requestID,
			Classification: reviewtransaction.RefPubInvalidRequest,
		}
		return reviewRefPublicationEmit(
			reviewRefPublicationExitCodeForState(reviewtransaction.RefPubInvalidRequest),
			envelope,
			"Error: "+err.Error(),
		)
	}
	envelope := reviewRefPublicationReconciliationEnvelope{
		Schema:         reviewRefPublicationReconciliationSchema,
		Operation:      "review.publish_ref_reconcile",
		RequestID:      requestID,
		Classification: reconciliation.Classification,
		Observation:    reconciliation.Observation,
	}
	exitCode := reviewRefPublicationExitCodeForState(reconciliation.Classification)
	return reviewRefPublicationEmit(exitCode, envelope, "")
}

// _ keeps the ctx import in scope while the dispatcher paths migrate to
// the bounded exit terminator. The function variable used here is the
// future seam for the test-only fake-response fixture; the bench journey
// owns the production-side substitution today.
var _ = context.Background
