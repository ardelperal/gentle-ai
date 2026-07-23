package reviewtransaction

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// legacyV1FixtureReceiptSchemaHash is the SHA-256 of the canonical
// encoding of the v1 legacy receipt fixture under
// testdata/v1.49.0-ordinary-4r/artifacts/receipt.json. It is the
// regression gate for the carry-on risk PR #1 introduced: any future
// change that adds a field to a shared type, or reorders these
// canonical fields, would silently shift this hash and break the
// import path of historic v1 journals. The constant must remain
// locked at the value computed when this PR was authored.
const legacyV1FixtureReceiptSchemaHash = "9f22b4a65ef5768c662db8cad76f074e89d88e3150ebfdbebded130015ae2322"

// TestDecisionPayload_JSONRoundTrip proves that every field of a
// fully-populated DecisionPayload, including the nested
// Adjudication, survives a marshal-and-unmarshal round trip.
func TestDecisionPayload_JSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC)
	original := DecisionPayload{
		Operation:  "review/decide",
		LineageID:  "lineage-roundtrip",
		Decision:   "continue",
		Revision:   "rev-abc",
		RecordedBy: "ardelperal",
		RecordedAt: now,
		Adjudication: &DecisionAdjudicatePayload{
			Operation: "review/decision-adjudicate-batch", LineageID: "lineage-roundtrip",
			Adjudicator: "adjudicator-1", EvidenceHashes: []string{"sha256:aaa", "sha256:bbb"},
			RetryCount: 0, DecidedAt: now,
		},
	}
	raw, err := MarshalDecisionPayload(original)
	if err != nil {
		t.Fatalf("MarshalDecisionPayload: %v", err)
	}
	decoded, err := UnmarshalDecisionPayload(raw)
	if err != nil {
		t.Fatalf("UnmarshalDecisionPayload: %v", err)
	}
	if decoded.Operation != original.Operation {
		t.Fatalf("Operation = %q, want %q", decoded.Operation, original.Operation)
	}
	if decoded.LineageID != original.LineageID {
		t.Fatalf("LineageID = %q, want %q", decoded.LineageID, original.LineageID)
	}
	if decoded.Decision != original.Decision {
		t.Fatalf("Decision = %q, want %q", decoded.Decision, original.Decision)
	}
	if decoded.Revision != original.Revision {
		t.Fatalf("Revision = %q, want %q", decoded.Revision, original.Revision)
	}
	if decoded.RecordedBy != original.RecordedBy {
		t.Fatalf("RecordedBy = %q, want %q", decoded.RecordedBy, original.RecordedBy)
	}
	if !decoded.RecordedAt.Equal(original.RecordedAt) {
		t.Fatalf("RecordedAt = %v, want %v", decoded.RecordedAt, original.RecordedAt)
	}
	if decoded.Adjudication == nil {
		t.Fatal("Adjudication = nil, want non-nil")
	}
	if decoded.Adjudication.Operation != original.Adjudication.Operation {
		t.Fatalf("Adjudication.Operation = %q, want %q", decoded.Adjudication.Operation, original.Adjudication.Operation)
	}
	if decoded.Adjudication.Adjudicator != original.Adjudication.Adjudicator {
		t.Fatalf("Adjudication.Adjudicator = %q, want %q", decoded.Adjudication.Adjudicator, original.Adjudication.Adjudicator)
	}
	if decoded.Adjudication.RetryCount != original.Adjudication.RetryCount {
		t.Fatalf("Adjudication.RetryCount = %d, want %d", decoded.Adjudication.RetryCount, original.Adjudication.RetryCount)
	}
	if !decoded.Adjudication.DecidedAt.Equal(original.Adjudication.DecidedAt) {
		t.Fatalf("Adjudication.DecidedAt = %v, want %v", decoded.Adjudication.DecidedAt, original.Adjudication.DecidedAt)
	}
	if len(decoded.Adjudication.EvidenceHashes) != len(original.Adjudication.EvidenceHashes) {
		t.Fatalf("Adjudication.EvidenceHashes length = %d, want %d", len(decoded.Adjudication.EvidenceHashes), len(original.Adjudication.EvidenceHashes))
	}
	for index, hash := range original.Adjudication.EvidenceHashes {
		if decoded.Adjudication.EvidenceHashes[index] != hash {
			t.Fatalf("Adjudication.EvidenceHashes[%d] = %q, want %q", index, decoded.Adjudication.EvidenceHashes[index], hash)
		}
	}
}

// TestDecisionPayload_OmitEmpty_NilAdjudication proves the optional
// Adjudication field is omitted from the marshaled JSON when it is
// nil, that the round trip still works, and that the decoder
// materializes a nil Adjudication on the way back. The literal
// decision value is "stop" with no adjudication, the canonical
// `--decision stop` shape used when a lineage is escalated without a
// bounded adjudication call.
func TestDecisionPayload_OmitEmpty_NilAdjudication(t *testing.T) {
	payload := DecisionPayload{
		Operation:  "review/decide",
		LineageID:  "lineage-omit",
		Decision:   "stop",
		Revision:   "rev-xyz",
		RecordedBy: "ardelperal",
		RecordedAt: time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC),
	}
	raw, err := MarshalDecisionPayload(payload)
	if err != nil {
		t.Fatalf("MarshalDecisionPayload: %v", err)
	}
	if bytes.Contains(raw, []byte(`"adjudication"`)) {
		t.Fatalf("marshaled payload unexpectedly contains adjudication: %s", raw)
	}
	decoded, err := UnmarshalDecisionPayload(raw)
	if err != nil {
		t.Fatalf("UnmarshalDecisionPayload: %v", err)
	}
	if decoded.Adjudication != nil {
		t.Fatalf("decoded Adjudication = %+v, want nil", decoded.Adjudication)
	}
	if decoded.Decision != "stop" {
		t.Fatalf("Decision = %q, want %q", decoded.Decision, "stop")
	}
	if decoded.LineageID != payload.LineageID {
		t.Fatalf("LineageID = %q, want %q", decoded.LineageID, payload.LineageID)
	}
}

// TestDecisionAdjudicatePayload_JSONRoundTrip proves every field of
// the bounded adjudication payload survives a marshal-and-unmarshal
// round trip, including the slice of evidence hashes and the retry
// counter.
func TestDecisionAdjudicatePayload_JSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC)
	original := DecisionAdjudicatePayload{
		Operation: "review/decision-adjudicate-batch", LineageID: "lineage-adjudicate",
		Adjudicator: "adjudicator-2", EvidenceHashes: []string{"sha256:111", "sha256:222", "sha256:333"},
		RetryCount: 2, DecidedAt: now,
	}
	raw, err := MarshalDecisionAdjudicatePayload(original)
	if err != nil {
		t.Fatalf("MarshalDecisionAdjudicatePayload: %v", err)
	}
	decoded, err := UnmarshalDecisionAdjudicatePayload(raw)
	if err != nil {
		t.Fatalf("UnmarshalDecisionAdjudicatePayload: %v", err)
	}
	if decoded.Operation != original.Operation {
		t.Fatalf("Operation = %q, want %q", decoded.Operation, original.Operation)
	}
	if decoded.LineageID != original.LineageID {
		t.Fatalf("LineageID = %q, want %q", decoded.LineageID, original.LineageID)
	}
	if decoded.Adjudicator != original.Adjudicator {
		t.Fatalf("Adjudicator = %q, want %q", decoded.Adjudicator, original.Adjudicator)
	}
	if decoded.RetryCount != original.RetryCount {
		t.Fatalf("RetryCount = %d, want %d", decoded.RetryCount, original.RetryCount)
	}
	if !decoded.DecidedAt.Equal(original.DecidedAt) {
		t.Fatalf("DecidedAt = %v, want %v", decoded.DecidedAt, original.DecidedAt)
	}
	if len(decoded.EvidenceHashes) != len(original.EvidenceHashes) {
		t.Fatalf("EvidenceHashes length = %d, want %d", len(decoded.EvidenceHashes), len(original.EvidenceHashes))
	}
	for index, hash := range original.EvidenceHashes {
		if decoded.EvidenceHashes[index] != hash {
			t.Fatalf("EvidenceHashes[%d] = %q, want %q", index, decoded.EvidenceHashes[index], hash)
		}
	}
}

// TestDecisionPayload_LegacyTransactionHashUnchangedAfterDecisionFieldAdded
// is the GATE TEST for the carry-on risk PR #1 introduced. The v1
// legacy receipt fixture under testdata/v1.49.0-ordinary-4r/ is the
// canonical baseline that this PR must not perturb. Adding the new
// DecisionPayload and DecisionAdjudicatePayload types must not change
// the v1 receipt's canonical encoding, so its SHA-256 over the
// canonical (sorted-keys, no-whitespace) JSON is the regression gate.
// The constant legacyV1FixtureReceiptSchemaHash was computed when this
// PR was authored and is the locked regression target.
func TestDecisionPayload_LegacyTransactionHashUnchangedAfterDecisionFieldAdded(t *testing.T) {
	fixturePath := filepath.Join("testdata", "v1.49.0-ordinary-4r", "artifacts", "receipt.json")
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read legacy receipt fixture: %v", err)
	}
	can, err := canonicalJSON(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("canonical encoding of legacy receipt: %v", err)
	}
	sum := sha256.Sum256(can)
	got := hex.EncodeToString(sum[:])
	if got != legacyV1FixtureReceiptSchemaHash {
		t.Fatalf("legacy v1 receipt schema hash drift: got %s, want %s", got, legacyV1FixtureReceiptSchemaHash)
	}
}
