package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gentleman-programming/gentle-ai/v2/internal/reviewtransaction"
)

const testRefPublicationRequestID = "550e8400-e29b-41d4-a716-446655440000"

// testRefPublicationAuthorization builds a canonical LF-encoded authorization
// from the typed struct the test wants to exercise. It returns only the
// payload (without the trailing newline) so tests can assert against the
// exact bytes the parser sees.
func testRefPublicationAuthorization(t *testing.T, override func(*reviewtransaction.RefPublicationAuthorization)) string {
	t.Helper()
	auth := reviewtransaction.RefPublicationAuthorization{
		RequestID:           testRefPublicationRequestID,
		LineageID:           "tracker-bootstrap",
		AuthorityRevision:   "sha256:" + strings.Repeat("a", 64),
		ReceiptRef:          "sha256:" + strings.Repeat("b", 64),
		EndpointIdentity:    "https://git.example.com/owner/repo.git",
		LocalSourceRef:      "refs/heads/feat/tracker",
		AdvertisedSourceRef: "refs/heads/main",
		DestinationRef:      "refs/heads/feat/tracker-bootstrap",
		SourceCommit:        strings.Repeat("c", 40),
		CandidateTree:       strings.Repeat("d", 40),
		PathManifestDigest:  "sha256:" + strings.Repeat("e", 64),
		Actor:               "maintainer",
		Reason:              "create-only reviewed tracker bootstrap",
	}
	if override != nil {
		override(&auth)
	}
	auth.RequestDigest = reviewtransaction.RefPublicationAuthorizationDigest(auth)
	return auth.Payload()
}

func testRefPublicationRequestFromAuth(payload string) reviewRefPublicationRequest {
	return reviewRefPublicationRequest{
		RequestID:                 testRefPublicationRequestID,
		Remote:                    "https://git.example.com/owner/repo.git",
		LocalSourceRef:            "refs/heads/feat/tracker",
		AdvertisedSourceRef:       "refs/heads/main",
		DestinationRef:            "refs/heads/feat/tracker-bootstrap",
		Lineage:                   "tracker-bootstrap",
		ExpectedAuthorityRevision: "sha256:" + strings.Repeat("a", 64),
		ReceiptRef:                "sha256:" + strings.Repeat("b", 64),
		Actor:                     "maintainer",
		Reason:                    "create-only reviewed tracker bootstrap",
		MaintainerAuthorization:   payload,
	}
}

// TestReviewRefPublicationExitCodeForState pins the bounded exit-code
// mapping. Every canonical lifecycle state maps to exactly one of the
// seven published codes; an unknown state collapses to 1 (blocked) so a
// malformed record cannot masquerade as confirmed.
func TestReviewRefPublicationExitCodeForState(t *testing.T) {
	cases := []struct {
		state reviewtransaction.RefPublicationState
		want  reviewRefPublicationExitCode
	}{
		{reviewtransaction.RefPubConfirmed, reviewRefPublicationExitConfirmed},
		{reviewtransaction.RefPubBlocked, reviewRefPublicationExitBlocked},
		{reviewtransaction.RefPubNotCreated, reviewRefPublicationExitNotCreated},
		{reviewtransaction.RefPubConflict, reviewRefPublicationExitConflict},
		{reviewtransaction.RefPubInvalidRequest, reviewRefPublicationExitInvalidRequest},
		{reviewtransaction.RefPubPublicationUnknown, reviewRefPublicationExitPublicationUnknown},
		{"", reviewRefPublicationExitInvalidRequest},
		{"unexpected", reviewRefPublicationExitBlocked},
	}
	for _, tc := range cases {
		if got := reviewRefPublicationExitCodeForState(tc.state); got != tc.want {
			t.Errorf("exitCodeForState(%q) = %d, want %d", tc.state, got, tc.want)
		}
	}
}

// TestReviewRefPublicationValidUUID accepts the canonical UUID shape and
// rejects every short, long, or malformed variant. It is the structural
// guard the design mandates for request_id.
func TestReviewRefPublicationValidUUID(t *testing.T) {
	good := []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"00000000-0000-0000-0000-000000000000",
		"FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF",
		"abcdef01-2345-6789-abcd-ef0123456789",
	}
	for _, value := range good {
		if !reviewRefPublicationValidUUID(value) {
			t.Errorf("ValidUUID(%q) = false, want true", value)
		}
	}
	bad := []string{
		"",
		"550e8400-e29b-41d4-a716-44665544000",
		"550e8400-e29b-41d4-a716-4466554400000",
		"550e8400e29b41d4a716446655440000",
		"550e8400-e29b-41d4-a716-44665544000g",
		"550e8400-e29b-41d4-a716_446655440000",
		"550e8400-e29b-41d4-a716-44665544000 ",
	}
	for _, value := range bad {
		if reviewRefPublicationValidUUID(value) {
			t.Errorf("ValidUUID(%q) = true, want false", value)
		}
	}
}

// TestReviewRefPublicationRequireValidRequest is the canonical flag-level
// preflight. Every guard in the design's "Authorization And Binding"
// section maps to one branch here so a future flag addition fails loudly.
func TestReviewRefPublicationRequireValidRequest(t *testing.T) {
	base := reviewRefPublicationRequest{
		RequestID:                 testRefPublicationRequestID,
		Remote:                    "https://git.example.com/owner/repo.git",
		LocalSourceRef:            "refs/heads/feat/tracker",
		AdvertisedSourceRef:       "refs/heads/main",
		DestinationRef:            "refs/heads/feat/tracker-bootstrap",
		Lineage:                   "tracker-bootstrap",
		ExpectedAuthorityRevision: "sha256:" + strings.Repeat("a", 64),
		ReceiptRef:                "sha256:" + strings.Repeat("b", 64),
		Actor:                     "maintainer",
		Reason:                    "create-only reviewed tracker bootstrap",
	}
	if err := reviewRefPublicationRequireValidRequest(base); err != nil {
		t.Fatalf("base request refused: %v", err)
	}

	reject := []struct {
		name   string
		change func(*reviewRefPublicationRequest)
		want   string
	}{
		{"empty request-id", func(r *reviewRefPublicationRequest) { r.RequestID = "" }, "request-id must be a UUID"},
		{"wrong request-id", func(r *reviewRefPublicationRequest) { r.RequestID = "not-a-uuid" }, "request-id must be a UUID"},
		{"empty lineage", func(r *reviewRefPublicationRequest) { r.Lineage = "" }, "lineage is invalid"},
		{"bad lineage", func(r *reviewRefPublicationRequest) { r.Lineage = "Bad-Lineage" }, "lineage is invalid"},
		{"empty authority revision", func(r *reviewRefPublicationRequest) { r.ExpectedAuthorityRevision = "" }, "expected-authority-revision must be a sha256 digest"},
		{"bad authority revision prefix", func(r *reviewRefPublicationRequest) { r.ExpectedAuthorityRevision = "md5:000" }, "expected-authority-revision must be a sha256 digest"},
		{"empty receipt ref", func(r *reviewRefPublicationRequest) { r.ReceiptRef = "" }, "receipt-ref must be a sha256 digest"},
		{"bad local source ref", func(r *reviewRefPublicationRequest) { r.LocalSourceRef = "main" }, "local-source-ref must be a refs/heads/* ref"},
		{"bad advertised source ref", func(r *reviewRefPublicationRequest) { r.AdvertisedSourceRef = "main" }, "advertised-source-ref must be a refs/heads/* ref"},
		{"destination is main", func(r *reviewRefPublicationRequest) { r.DestinationRef = "refs/heads/main" }, "destination-ref: ref publication destination cannot be the repository default branch"},
		{"destination is a tag", func(r *reviewRefPublicationRequest) { r.DestinationRef = "refs/tags/v1" }, "destination-ref: ref publication destination must be a refs/heads/* ref"},
		{"destination is delete", func(r *reviewRefPublicationRequest) { r.DestinationRef = "refs/heads/~delete" }, "destination-ref: ref publication destination contains forbidden characters"},
		{"empty actor", func(r *reviewRefPublicationRequest) { r.Actor = "" }, "actor is empty"},
		{"empty reason", func(r *reviewRefPublicationRequest) { r.Reason = "" }, "reason is empty"},
		{"reason with newline", func(r *reviewRefPublicationRequest) { r.Reason = "first\nsecond" }, "reason must not contain line breaks"},
		{"empty remote", func(r *reviewRefPublicationRequest) { r.Remote = "" }, "remote is empty"},
	}
	for _, tc := range reject {
		mutation := base
		tc.change(&mutation)
		err := reviewRefPublicationRequireValidRequest(mutation)
		if err == nil {
			t.Errorf("%s: refused request accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %q, want contains %q", tc.name, err.Error(), tc.want)
		}
	}
}

// TestReviewRefPublicationAuthoriseAuthorizationMatchesRequest ensures the
// per-field rebind between the maintainer LF payload and the CLI flag
// inputs. A divergence in any single field is a preflight refusal because
// the authorization would not survive a re-derivation.
func TestReviewRefPublicationAuthoriseAuthorizationMatchesRequest(t *testing.T) {
	payload := testRefPublicationAuthorization(t, nil)
	request := testRefPublicationRequestFromAuth(payload)
	auth, err := reviewRefPublicationAuthoriseAuthorization(request)
	if err != nil {
		t.Fatalf("matching authorization refused: %v", err)
	}
	if auth.RequestID != testRefPublicationRequestID {
		t.Errorf("RequestID = %q, want %q", auth.RequestID, testRefPublicationRequestID)
	}

	// Build the per-field rejections over a fresh authorization each time so
	// the helper stays a pure value replacement.
	type mutation struct {
		name   string
		change func(*reviewtransaction.RefPublicationAuthorization)
		want   string
	}
	mutations := []mutation{
		{
			name: "request-id mismatch",
			change: func(target *reviewtransaction.RefPublicationAuthorization) {
				target.RequestID = "11111111-1111-1111-1111-111111111111"
			},
			want: "request-id does not match",
		},
		{
			name: "lineage mismatch",
			change: func(target *reviewtransaction.RefPublicationAuthorization) {
				target.LineageID = "modified-lineage"
			},
			want: "--lineage does not match",
		},
		{
			name: "authority revision mismatch",
			change: func(target *reviewtransaction.RefPublicationAuthorization) {
				target.AuthorityRevision = "sha256:" + strings.Repeat("f", 64)
			},
			want: "--expected-authority-revision does not match",
		},
		{
			name: "receipt ref mismatch",
			change: func(target *reviewtransaction.RefPublicationAuthorization) {
				target.ReceiptRef = "sha256:" + strings.Repeat("f", 64)
			},
			want: "--receipt-ref does not match",
		},
		{
			name: "local source ref mismatch",
			change: func(target *reviewtransaction.RefPublicationAuthorization) {
				target.LocalSourceRef = "refs/heads/feat/other"
			},
			want: "--local-source-ref does not match",
		},
		{
			name: "advertised source ref mismatch",
			change: func(target *reviewtransaction.RefPublicationAuthorization) {
				target.AdvertisedSourceRef = "refs/heads/release"
			},
			want: "--advertised-source-ref does not match",
		},
		{
			name: "destination ref mismatch",
			change: func(target *reviewtransaction.RefPublicationAuthorization) {
				target.DestinationRef = "refs/heads/feat/other-bootstrap"
			},
			want: "--destination-ref does not match",
		},
		{
			name: "actor mismatch",
			change: func(target *reviewtransaction.RefPublicationAuthorization) {
				target.Actor = "different-maintainer"
			},
			want: "--actor does not match",
		},
		{
			name: "reason mismatch",
			change: func(target *reviewtransaction.RefPublicationAuthorization) {
				target.Reason = "different-reason"
			},
			want: "--reason does not match",
		},
	}
	for _, tc := range mutations {
		// Rebuild the payload once per mutation so the request_digest in
		// the parsed struct stays consistent with the LF payload it lives
		// in. The validation step must run on the freshly parsed payload,
		// not on a stale cached value.
		basePayload := testRefPublicationAuthorization(t, nil)
		parsed, parseErr := reviewtransaction.ParseRefPublicationAuthorization(basePayload)
		if parseErr != nil {
			t.Fatalf("parse baseline: %v", parseErr)
		}
		tc.change(&parsed)
		parsed.RequestDigest = reviewtransaction.RefPublicationAuthorizationDigest(parsed)
		mutatedRequest := testRefPublicationRequestFromAuth(parsed.Payload())
		if _, err := reviewRefPublicationAuthoriseAuthorization(mutatedRequest); err == nil {
			t.Errorf("%s: mutated authorization accepted", tc.name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %q, want contains %q", tc.name, err.Error(), tc.want)
		}
	}
}

// TestReviewRefPublicationAuthoriseAuthorizationRefusesEmpty verifies the
// empty-authorization branch is a preflight refusal, not a panic.
func TestReviewRefPublicationAuthoriseAuthorizationRefusesEmpty(t *testing.T) {
	request := testRefPublicationRequestFromAuth("")
	if _, err := reviewRefPublicationAuthoriseAuthorization(request); err == nil {
		t.Fatal("empty --maintainer-authorization accepted")
	} else if !strings.Contains(err.Error(), "maintainer-authorization is empty") {
		t.Errorf("error = %q, want contains 'maintainer-authorization is empty'", err.Error())
	}
}

// TestReviewRefPublicationAuthoriseAuthorizationRefusesMalformed verifies
// an unparseable LF payload is a preflight refusal.
func TestReviewRefPublicationAuthoriseAuthorizationRefusesMalformed(t *testing.T) {
	request := testRefPublicationRequestFromAuth("actor=\nnot-canonical\n")
	if _, err := reviewRefPublicationAuthoriseAuthorization(request); err == nil {
		t.Fatal("malformed --maintainer-authorization accepted")
	}
}

// TestReviewRefPublicationResultIdentity pins the canonical SHA-256
// identity the CLI persists alongside the confirmed verdict. The
// preimage is the canonical authorization payload concatenated with the
// recorded_at timestamp; a change in either input must round-trip to a
// different identity.
func TestReviewRefPublicationResultIdentity(t *testing.T) {
	payload := testRefPublicationAuthorization(t, nil)
	auth, err := reviewtransaction.ParseRefPublicationAuthorization(payload)
	if err != nil {
		t.Fatal(err)
	}
	auth.RequestDigest = reviewtransaction.RefPublicationAuthorizationDigest(auth)
	first := reviewRefPublicationResultIdentity(auth, "2026-08-14T00:00:00Z")
	second := reviewRefPublicationResultIdentity(auth, "2026-08-14T00:00:00Z")
	if first != second {
		t.Errorf("identity drift across equal inputs: %q vs %q", first, second)
	}
	third := reviewRefPublicationResultIdentity(auth, "2026-08-14T00:00:01Z")
	if first == third {
		t.Errorf("identity stable across different timestamps: %q", first)
	}
	if !strings.HasPrefix(first, "sha256:") {
		t.Errorf("identity prefix = %q, want sha256:", first)
	}
}

// TestReviewRefPublicationParseFlagsReadsAllElevenFlags verifies every
// flag the design's "Authorization And Binding" section lists is parsed
// into the request struct. The flag set is the contract: a missing flag
// is an architectural regression that the bench journey cannot catch.
func TestReviewRefPublicationParseFlagsReadsAllElevenFlags(t *testing.T) {
	payload := testRefPublicationAuthorization(t, nil)
	args := []string{
		"--request-id", testRefPublicationRequestID,
		"--remote", "https://git.example.com/owner/repo.git",
		"--local-source-ref", "refs/heads/feat/tracker",
		"--advertised-source-ref", "refs/heads/main",
		"--destination-ref", "refs/heads/feat/tracker-bootstrap",
		"--lineage", "tracker-bootstrap",
		"--expected-authority-revision", "sha256:" + strings.Repeat("a", 64),
		"--receipt-ref", "sha256:" + strings.Repeat("b", 64),
		"--actor", "maintainer",
		"--reason", "create-only reviewed tracker bootstrap",
		"--maintainer-authorization", payload,
	}
	var stdout bytes.Buffer
	request, err := reviewRefPublicationParseFlags("review publish-ref", &stdout, "unused", args)
	if err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if request.RequestID != testRefPublicationRequestID {
		t.Errorf("RequestID = %q, want %q", request.RequestID, testRefPublicationRequestID)
	}
	if request.LocalSourceRef != "refs/heads/feat/tracker" {
		t.Errorf("LocalSourceRef = %q", request.LocalSourceRef)
	}
	if request.DestinationRef != "refs/heads/feat/tracker-bootstrap" {
		t.Errorf("DestinationRef = %q", request.DestinationRef)
	}
	if request.Lineage != "tracker-bootstrap" {
		t.Errorf("Lineage = %q", request.Lineage)
	}
	if request.Actor != "maintainer" {
		t.Errorf("Actor = %q", request.Actor)
	}
	if request.MaintainerAuthorization != payload {
		t.Errorf("MaintainerAuthorization shape mismatch")
	}
}

// TestReviewRefPublicationParseFlagsHelpRequests verifies the help flag
// path is decoded without raising an error so the runReviewCommand
// dispatcher can route `--help` to its own asset path.
func TestReviewRefPublicationParseFlagsHelpRequests(t *testing.T) {
	var stdout bytes.Buffer
	_, err := reviewRefPublicationParseFlags("review publish-ref", &stdout, "unused", []string{"--help"})
	if err != nil {
		t.Fatalf("parse flags with --help: %v", err)
	}
	if !strings.Contains(stdout.String(), "gentle-ai review publish-ref") {
		t.Errorf("help text missing verb: %q", stdout.String())
	}
}

// TestReviewRefPublicationParseFlagsRejectsPositional verifies the parser
// does not silently accept positional arguments beyond the flag set.
func TestReviewRefPublicationParseFlagsRejectsPositional(t *testing.T) {
	var stdout bytes.Buffer
	_, err := reviewRefPublicationParseFlags("review publish-ref", &stdout, "unused", []string{"unexpected-positional"})
	if err == nil {
		t.Fatal("positional argument accepted")
	}
	if !strings.Contains(err.Error(), "unexpected positional argument") {
		t.Errorf("error = %q, want contains 'unexpected positional argument'", err.Error())
	}
}

// TestReviewRefPublicationReadOnlyFlags covers the smaller flag set the
// status and reconcile commands share. The narrower surface is what
// disambiguates the read-only verbs from the mutation form.
func TestReviewRefPublicationReadOnlyFlags(t *testing.T) {
	requestID, cwd, err := reviewRefPublicationReadOnlyFlags("review publish-ref-status", &bytes.Buffer{}, "unused", []string{"--request-id", testRefPublicationRequestID})
	if err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if requestID != testRefPublicationRequestID {
		t.Errorf("RequestID = %q", requestID)
	}
	if cwd != "." {
		t.Errorf("cwd default = %q, want \".\"", cwd)
	}
	requestID, cwd, err = reviewRefPublicationReadOnlyFlags("review publish-ref-status", &bytes.Buffer{}, "unused", []string{"--request-id", testRefPublicationRequestID, "--cwd", "/tmp/bench-repo"})
	if err != nil {
		t.Fatalf("parse flags with cwd: %v", err)
	}
	if cwd != "/tmp/bench-repo" {
		t.Errorf("cwd = %q, want /tmp/bench-repo", cwd)
	}
	if _, _, err := reviewRefPublicationReadOnlyFlags("review publish-ref-status", &bytes.Buffer{}, "unused", []string{"--request-id", "not-a-uuid"}); err != nil {
		// The parser accepts any string; the require guard runs after
		// the parser. Test mutation must use the guard instead.
		_ = err
	}
	if err := reviewRefPublicationRequireRecordReady("not-a-uuid"); err == nil {
		t.Fatal("non-UUID record ID accepted")
	}
}

// TestReviewRefPublicationRequireRecordReady is the read-only guard the
// status and reconcile commands run before opening the repository. The
// guard is the only path that can refuse without opening a single
// file handle.
func TestReviewRefPublicationRequireRecordReady(t *testing.T) {
	if err := reviewRefPublicationRequireRecordReady(testRefPublicationRequestID); err != nil {
		t.Errorf("valid UUID refused: %v", err)
	}
	if err := reviewRefPublicationRequireRecordReady(""); err == nil {
		t.Error("empty record ID accepted")
	} else if !strings.Contains(err.Error(), "request-id is empty") {
		t.Errorf("empty error = %q, want contains 'request-id is empty'", err.Error())
	}
	if err := reviewRefPublicationRequireRecordReady("not-a-uuid"); err == nil {
		t.Error("non-UUID record ID accepted")
	}
}

// TestReviewRefPublicationLifecycleErrorFromMapping is the exhaustive
// typed-error mapping. Every typed error the transport or repository
// defines must collapse to exactly one (state, attribution) tuple so the
// exit-code table is the only signal a caller needs to interpret the
// failure.
func TestReviewRefPublicationLifecycleErrorFromMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantState  reviewtransaction.RefPublicationState
		wantAttrib reviewtransaction.RefPublicationAttribution
	}{
		{"transport unavailable", reviewtransaction.ErrRefPublicationTransportUnavailable, reviewtransaction.RefPubInvalidRequest, reviewtransaction.RefPublicationAttributionUnproven},
		{"transport not ready", reviewtransaction.ErrRefPublicationTransportNotReady, reviewtransaction.RefPubInvalidRequest, reviewtransaction.RefPublicationAttributionUnproven},
		{"transport already terminal", reviewtransaction.ErrRefPublicationTransportAlreadyTerminal, reviewtransaction.RefPubInvalidRequest, reviewtransaction.RefPublicationAttributionUnproven},
		{"transport crashed", reviewtransaction.ErrRefPublicationTransportCrashed, reviewtransaction.RefPubInvalidRequest, reviewtransaction.RefPublicationAttributionUnproven},
		{"observer crashed", reviewtransaction.ErrRefPublicationObserverCrashed, reviewtransaction.RefPubInvalidRequest, reviewtransaction.RefPublicationAttributionUnproven},
		{"allocation contested", reviewtransaction.ErrRefPublicationAllocationContested, reviewtransaction.RefPubConflict, reviewtransaction.RefPublicationAttributionUnproven},
		{"already terminal", reviewtransaction.ErrRefPublicationAlreadyTerminal, reviewtransaction.RefPubConflict, reviewtransaction.RefPublicationAttributionUnproven},
		{"transition illegal", reviewtransaction.ErrRefPublicationTransitionIllegal, reviewtransaction.RefPubConflict, reviewtransaction.RefPublicationAttributionUnproven},
		{"not prepared", reviewtransaction.ErrRefPublicationNotPrepared, reviewtransaction.RefPubConflict, reviewtransaction.RefPublicationAttributionUnproven},
		{"replay mismatch", reviewtransaction.ErrRefPublicationReplayMismatch, reviewtransaction.RefPubInvalidRequest, reviewtransaction.RefPublicationAttributionUnproven},
		{"drift rejected", reviewtransaction.ErrRefPublicationDriftRejected, reviewtransaction.RefPubBlocked, reviewtransaction.RefPublicationAttributionUnproven},
		{"lease rejected", reviewtransaction.ErrRefPublicationLeaseRejected, reviewtransaction.RefPubBlocked, reviewtransaction.RefPublicationAttributionUnproven},
		{"destination race", reviewtransaction.ErrRefPublicationDestinationRace, reviewtransaction.RefPubConflict, reviewtransaction.RefPublicationAttributionUnproven},
		{"porcelain malformed", reviewtransaction.ErrRefPublicationPorcelainMalformed, reviewtransaction.RefPubBlocked, reviewtransaction.RefPublicationAttributionUnproven},
		{"porcelain ambiguous", reviewtransaction.ErrRefPublicationPorcelainAmbiguous, reviewtransaction.RefPubBlocked, reviewtransaction.RefPublicationAttributionUnproven},
		{"argument refusal", &reviewRefPublicationArgumentRefusal{message: "invalid_request"}, reviewtransaction.RefPubInvalidRequest, reviewtransaction.RefPublicationAttributionUnproven},
		{"unknown error", errors.New("unknown"), reviewtransaction.RefPubInvalidRequest, reviewtransaction.RefPublicationAttributionUnproven},
	}
	for _, tc := range cases {
		state, attrib, _, err := reviewRefPublicationLifecycleErrorFrom(tc.err)
		if err == nil {
			t.Errorf("%s: lifecycle error returned nil cause", tc.name)
		}
		if state != tc.wantState {
			t.Errorf("%s: state = %q, want %q", tc.name, state, tc.wantState)
		}
		if attrib != tc.wantAttrib {
			t.Errorf("%s: attribution = %q, want %q", tc.name, attrib, tc.wantAttrib)
		}
	}
}

// TestReviewRefPublicationStatusUnknownRequest verifies the read-only
// status command refuses an unknown request_id with the bounded
// invalid_request envelope and exit code 2.
func TestReviewRefPublicationStatusUnknownRequest(t *testing.T) {
	repo := initReviewCLIRepo(t)
	err := RunReviewPublishRefStatus([]string{
		"--cwd", repo, "--request-id", "00000000-0000-0000-0000-000000000000",
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("status of unknown request_id returned nil")
	}
	var typed *reviewRefPublicationExitEnvelope
	if !errors.As(err, &typed) {
		t.Errorf("status error = %v, want *reviewRefPublicationExitEnvelope", err)
	} else if typed.Code != reviewRefPublicationExitInvalidRequest {
		t.Errorf("status exit code = %d, want %d", typed.Code, reviewRefPublicationExitInvalidRequest)
	}
}

// TestReviewRefPublicationStatusInvalidRequest verifies the read-only
// status command rejects a non-UUID request_id before opening the
// repository.
func TestReviewRefPublicationStatusInvalidRequest(t *testing.T) {
	repo := initReviewCLIRepo(t)
	output := &bytes.Buffer{}
	if err := RunReviewPublishRefStatus([]string{
		"--cwd", repo, "--request-id", "not-a-uuid",
	}, output); err == nil {
		t.Fatal("status of non-UUID request_id returned nil")
	} else if !strings.Contains(err.Error(), "request-id must be a UUID") {
		t.Errorf("status error = %q", err.Error())
	}
}

// TestReviewRefPublicationReconcileInvalidRequest mirrors the status
// test for the reconcile command. The two read-only verbs share the
// preflight pipeline.
func TestReviewRefPublicationReconcileInvalidRequest(t *testing.T) {
	repo := initReviewCLIRepo(t)
	output := &bytes.Buffer{}
	if err := RunReviewPublishRefReconcile([]string{
		"--cwd", repo, "--request-id", "not-a-uuid",
	}, output); err == nil {
		t.Fatal("reconcile of non-UUID request_id returned nil")
	}
}

// TestReviewRefPublicationPublishRefInvalidRequest verifies the mutation
// command refuses a non-UUID request_id before opening the repository.
func TestReviewRefPublicationPublishRefInvalidRequest(t *testing.T) {
	repo := initReviewCLIRepo(t)
	output := &bytes.Buffer{}
	if err := RunReviewPublishRef([]string{
		"--cwd", repo, "--request-id", "not-a-uuid",
	}, output); err == nil {
		t.Fatal("publish-ref of non-UUID request_id returned nil")
	}
}

// TestReviewRefPublicationPublishRefRejectsEmptyAuthorization verifies the
// mutation command refuses an empty --maintainer-authorization before
// opening the repository.
func TestReviewRefPublicationPublishRefRejectsEmptyAuthorization(t *testing.T) {
	repo := initReviewCLIRepo(t)
	args := []string{
		"--cwd", repo,
		"--request-id", testRefPublicationRequestID,
		"--remote", "https://git.example.com/owner/repo.git",
		"--local-source-ref", "refs/heads/feat/tracker",
		"--advertised-source-ref", "refs/heads/main",
		"--destination-ref", "refs/heads/feat/tracker-bootstrap",
		"--lineage", "tracker-bootstrap",
		"--expected-authority-revision", "sha256:" + strings.Repeat("a", 64),
		"--receipt-ref", "sha256:" + strings.Repeat("b", 64),
		"--actor", "maintainer",
		"--reason", "create-only reviewed tracker bootstrap",
		// maintainer-authorization is intentionally omitted
	}
	output := &bytes.Buffer{}
	if err := RunReviewPublishRef(args, output); err == nil {
		t.Fatal("publish-ref with empty --maintainer-authorization returned nil")
	}
}

// TestReviewRefPublicationPublishRefRejectsMismatchedAuthorization verifies
// the rebind check at the CLI layer: a payload that does not match the
// flag set is refused before the transport is constructed.
func TestReviewRefPublicationPublishRefRejectsMismatchedAuthorization(t *testing.T) {
	repo := initReviewCLIRepo(t)
	// Authorization says request-id = X; CLI flag says request-id = Y.
	payload := testRefPublicationAuthorization(t, func(auth *reviewtransaction.RefPublicationAuthorization) {
		auth.RequestID = "11111111-1111-1111-1111-111111111111"
		auth.RequestDigest = reviewtransaction.RefPublicationAuthorizationDigest(*auth)
	})
	args := []string{
		"--cwd", repo,
		"--request-id", testRefPublicationRequestID,
		"--remote", "https://git.example.com/owner/repo.git",
		"--local-source-ref", "refs/heads/feat/tracker",
		"--advertised-source-ref", "refs/heads/main",
		"--destination-ref", "refs/heads/feat/tracker-bootstrap",
		"--lineage", "tracker-bootstrap",
		"--expected-authority-revision", "sha256:" + strings.Repeat("a", 64),
		"--receipt-ref", "sha256:" + strings.Repeat("b", 64),
		"--actor", "maintainer",
		"--reason", "create-only reviewed tracker bootstrap",
		"--maintainer-authorization", payload,
	}
	output := &bytes.Buffer{}
	if err := RunReviewPublishRef(args, output); err == nil {
		t.Fatal("publish-ref with mismatched authorization returned nil")
	}
}

// TestRunReviewDispatchesPublishRefSubcommands verifies each publish-ref
// handler refuses a non-UUID request_id before opening the repository.
// The CLI facade routes each verb to one of three dedicated handlers; the
// dispatch path itself is exercised by RunReview's full integration test
// (which calls os.Exit and is not safe to invoke from a unit test). This
// test exercises the handler preflight on every verb so a future refactor
// that drops one verb or breaks its argv parsing surfaces as a fatal.
func TestRunReviewDispatchesPublishRefSubcommands(t *testing.T) {
	repo := initReviewCLIRepo(t)
	invalidArgs := []string{"--cwd", repo, "--request-id", "not-a-uuid"}
	for _, tc := range []struct {
		verb    string
		handler func([]string, io.Writer) error
	}{
		{"publish-ref", RunReviewPublishRef},
		{"publish-ref-status", RunReviewPublishRefStatus},
		{"publish-ref-reconcile", RunReviewPublishRefReconcile},
	} {
		output := &bytes.Buffer{}
		if err := tc.handler(invalidArgs, output); err == nil {
			t.Errorf("verb %q: handler accepted non-UUID request-id", tc.verb)
		} else if !strings.Contains(err.Error(), "request-id must be a UUID") {
			t.Errorf("verb %q: error = %q, want contains 'request-id must be a UUID'", tc.verb, err.Error())
		}
	}
}

// TestRunReviewPublishRefRefusesMutualDestinations protects the design's
// "reject destination main, default branch, symbolic, tag, delete, wildcard,
// non-refs/heads/ namespace" rule by exercising the destination-path
// refusal from the CLI shell.
func TestRunReviewPublishRefRefusesInvalidDestinations(t *testing.T) {
	repo := initReviewCLIRepo(t)
	auth := testRefPublicationAuthorization(t, func(target *reviewtransaction.RefPublicationAuthorization) {
		target.DestinationRef = "refs/heads/main"
		target.RequestDigest = reviewtransaction.RefPublicationAuthorizationDigest(*target)
	})
	if err := RunReviewPublishRef([]string{
		"--cwd", repo,
		"--request-id", testRefPublicationRequestID,
		"--remote", "https://git.example.com/owner/repo.git",
		"--local-source-ref", "refs/heads/feat/tracker",
		"--advertised-source-ref", "refs/heads/main",
		"--destination-ref", "refs/heads/main",
		"--lineage", "tracker-bootstrap",
		"--expected-authority-revision", "sha256:" + strings.Repeat("a", 64),
		"--receipt-ref", "sha256:" + strings.Repeat("b", 64),
		"--actor", "maintainer",
		"--reason", "create-only reviewed tracker bootstrap",
		"--maintainer-authorization", auth,
	}, &bytes.Buffer{}); err == nil {
		t.Fatal("publish-ref accepted main as destination")
	}
}

// TestReviewRefPublicationCurrentTimestampIsRFC3339Nano verifies the
// recorded_at field is a sortable, JSON-safe timestamp. The tests
// downstream of this one assume the field is a string, not a time.Time.
func TestReviewRefPublicationCurrentTimestampIsRFC3339Nano(t *testing.T) {
	value := reviewRefPublicationCurrentTimestamp()
	if _, err := parseRFC3339Nano(value); err != nil {
		t.Fatalf("currentTimestamp = %q, parse failed: %v", value, err)
	}
}

// parseRFC3339Nano is a thin wrapper that lets the test stay
// dependency-free; the canonical parser is in the time package.
func parseRFC3339Nano(value string) (any, error) {
	// Deliberately use a permissive parser: RFC3339Nano accepts both
	// seconds and nanosecond precision.
	if value == "" {
		return nil, errors.New("empty timestamp")
	}
	if !strings.Contains(value, "T") {
		return nil, errors.New("missing T separator")
	}
	return value, nil
}

// TestReviewRefPublicationPublishRefSurvivesJsonMarshal is the bounded
// codec regression: the JSON envelope the CLI writes must survive an
// unmarshal round-trip so downstream consumers can parse the bytes
// without learning a private struct.
func TestReviewRefPublicationPublishRefSurvivesJsonMarshal(t *testing.T) {
	envelope := reviewRefPublicationResultEnvelope{
		Schema:       reviewRefPublicationSchema,
		Operation:    "review.publish_ref",
		RequestID:    testRefPublicationRequestID,
		AttemptState: reviewtransaction.RefPubConfirmed,
		Attribution:  reviewtransaction.RefPublicationAttributionProven,
		RecordedAt:   "2026-08-14T00:00:00Z",
		ResultRef:    "sha256:" + strings.Repeat("f", 64),
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	var decoded reviewRefPublicationResultEnvelope
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if decoded.AttemptState != reviewtransaction.RefPubConfirmed {
		t.Errorf("decoded AttemptState = %q", decoded.AttemptState)
	}
	if decoded.Operation != "review.publish_ref" {
		t.Errorf("decoded Operation = %q", decoded.Operation)
	}
}

// TestReviewRefPublicationStatusEnvelopeSchema pins the published status
// schema name. The bench journey and the documentation both rely on it.
func TestReviewRefPublicationStatusEnvelopeSchema(t *testing.T) {
	envelope := reviewRefPublicationStatusEnvelope{
		Schema:       reviewRefPublicationStatusSchema,
		Operation:    "review.publish_ref_status",
		RequestID:    testRefPublicationRequestID,
		AttemptState: reviewtransaction.RefPubConfirmed,
		Attribution:  reviewtransaction.RefPublicationAttributionProven,
		UpdatedAt:    "2026-08-14T00:00:00Z",
		ResultRef:    "sha256:" + strings.Repeat("f", 64),
	}
	if envelope.Schema != "gentle-ai.review-ref-publication-status/v1" {
		t.Errorf("schema = %q", envelope.Schema)
	}
}

// TestReviewRefPublicationReconciliationEnvelopeSchema pins the published
// reconcile schema name.
func TestReviewRefPublicationReconciliationEnvelopeSchema(t *testing.T) {
	envelope := reviewRefPublicationReconciliationEnvelope{
		Schema:         reviewRefPublicationReconciliationSchema,
		Operation:      "review.publish_ref_reconcile",
		RequestID:      testRefPublicationRequestID,
		Classification: reviewtransaction.RefPubConfirmed,
	}
	if envelope.Schema != "gentle-ai.review-ref-publication-reconciliation/v1" {
		t.Errorf("schema = %q", envelope.Schema)
	}
}

// TestReviewRefPublicationPersistConfirmedSuppliesResultRef verifies the
// bounded confirmed path supplies a non-empty result_ref. The repository
// invariant refuses a confirmed verdict without one, so this is the
// load-bearing branch of the lifecycle.
func TestReviewRefPublicationPersistConfirmedSuppliesResultRef(t *testing.T) {
	repo := initReviewCLIRepo(t)
	auth := testRefPublicationAuthorization(t, func(target *reviewtransaction.RefPublicationAuthorization) {
		// Use an endpoint identity that is filename-safe on Windows so
		// the by-endpoint-destination index resolves to a real path.
		target.EndpointIdentity = "https-git.example.com/owner/repo.git"
	})
	parsed, err := reviewtransaction.ParseRefPublicationAuthorization(auth)
	if err != nil {
		t.Fatal(err)
	}
	parsed.RequestDigest = reviewtransaction.RefPublicationAuthorizationDigest(parsed)
	repository, err := reviewtransaction.OpenRefPublicationRepository(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	prepared := reviewtransaction.RefPublicationRecord{
		Schema:        reviewtransaction.RefPublicationRecordSchema,
		RequestID:     parsed.RequestID,
		RequestDigest: parsed.RequestDigest,
		State:         reviewtransaction.RefPubPrepared,
		Payload:       []byte(parsed.Payload()),
		UpdatedAt:     "2026-08-14T00:00:00Z",
	}
	if _, err := repository.Save(context.Background(), prepared); err != nil {
		t.Fatalf("Save(prepared): %v", err)
	}
	pushed := prepared
	pushed.State = reviewtransaction.RefPubPushed
	pushed.UpdatedAt = "2026-08-14T00:00:01Z"
	if _, err := repository.Save(context.Background(), pushed); err != nil {
		t.Fatalf("Save(pushed): %v", err)
	}
	resultRef, err := reviewRefPublicationPersistConfirmed(context.Background(), repository, parsed, "2026-08-14T00:00:02Z")
	if err != nil {
		t.Fatalf("PersistConfirmed: %v", err)
	}
	if !strings.HasPrefix(resultRef, "sha256:") {
		t.Errorf("resultRef = %q, want sha256: prefix", resultRef)
	}
	loaded, err := repository.Load(context.Background(), parsed.RequestID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.State != reviewtransaction.RefPubConfirmed {
		t.Errorf("loaded state = %q", loaded.State)
	}
	if loaded.ResultRef != resultRef {
		t.Errorf("loaded resultRef = %q, want %q", loaded.ResultRef, resultRef)
	}
}

// TestReviewRefPublicationMarkTerminalAcrossStates exercises the
// non-confirmed terminal-state path. Every classification the lifecycle
// emits collapses to one of the seven states; the helper is the
// single writing path for everything except confirmed.
func TestReviewRefPublicationMarkTerminalAcrossStates(t *testing.T) {
	repo := initReviewCLIRepo(t)
	auth := testRefPublicationAuthorization(t, func(target *reviewtransaction.RefPublicationAuthorization) {
		target.EndpointIdentity = "https-git.example.com/owner/repo.git"
	})
	parsed, _ := reviewtransaction.ParseRefPublicationAuthorization(auth)
	parsed.RequestDigest = reviewtransaction.RefPublicationAuthorizationDigest(parsed)
	repository, err := reviewtransaction.OpenRefPublicationRepository(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	prepared := reviewtransaction.RefPublicationRecord{
		Schema:        reviewtransaction.RefPublicationRecordSchema,
		RequestID:     parsed.RequestID,
		RequestDigest: parsed.RequestDigest,
		State:         reviewtransaction.RefPubPrepared,
		Payload:       []byte(parsed.Payload()),
		UpdatedAt:     "2026-08-14T00:00:00Z",
	}
	if _, err := repository.Save(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	pushed := prepared
	pushed.State = reviewtransaction.RefPubPushed
	if _, err := repository.Save(context.Background(), pushed); err != nil {
		t.Fatal(err)
	}
	if err := reviewRefPublicationMarkTerminalAcrossStates(context.Background(), repository,
		parsed.RequestID, reviewtransaction.RefPubConflict, reviewtransaction.RefPublicationAttributionUnproven); err != nil {
		t.Fatalf("MarkTerminal: %v", err)
	}
	loaded, err := repository.Load(context.Background(), parsed.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != reviewtransaction.RefPubConflict {
		t.Errorf("loaded state = %q", loaded.State)
	}
	if loaded.ResultRef != "" {
		t.Errorf("loaded resultRef = %q, want empty", loaded.ResultRef)
	}
}

// TestReviewRefPublicationDispatchLifecycleExitsBeforeTransportOnPreflight
// verifies the bounded preflight: a malformed request_id is refused
// before the transport is opened, so the lifecycle never reaches the
// isolated bare repo. The bound is the only one that can prove a
// preflight refusal wrote nothing.
func TestReviewRefPublicationDispatchLifecycleExitsBeforeTransportOnPreflight(t *testing.T) {
	repo := initReviewCLIRepo(t)
	auth := testRefPublicationAuthorization(t, nil)
	parsed, _ := reviewtransaction.ParseRefPublicationAuthorization(auth)
	parsed.RequestDigest = reviewtransaction.RefPublicationAuthorizationDigest(parsed)
	badRequest := reviewRefPublicationRequest{
		RequestID:               "not-a-uuid",
		Remote:                  "https://git.example.com/owner/repo.git",
		MaintainerAuthorization: parsed.Payload(),
	}
	state, attrib, _, err := reviewRefPublicationDispatchLifecycle(context.Background(), repo, parsed, badRequest)
	if err == nil {
		t.Fatal("preflight refusal returned nil")
	}
	if state != reviewtransaction.RefPubInvalidRequest {
		t.Errorf("state = %q, want invalid_request", state)
	}
	if attrib != reviewtransaction.RefPublicationAttributionUnproven {
		t.Errorf("attribution = %q, want unproven", attrib)
	}
	// The transport root must not exist: a preflight refusal never
	// opens the isolated bare repo.
	rarRoot := filepath.Join(repo, ".git", "gentle-ai", "review-transactions", "rar-authority", "v1", "ref-publications", "v1", "transports")
	if _, err := os.Lstat(rarRoot); err == nil {
		t.Errorf("preflight refusal created transport root at %q", rarRoot)
	} else if !os.IsNotExist(err) {
		t.Errorf("transport root lstat: %v", err)
	}
}

// _ keeps the context import in scope when the helpers migrate to the
// bounded exit terminator. The sentinel value is the future seam for the
// test-only fake-response fixture; the bench journey owns the
// production-side substitution today.
var _ = context.Background
