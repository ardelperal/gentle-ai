package reviewtransaction

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Schema constants for the v2 decision-required payload envelope. The
// schema literal is the externally-visible contract used by the journal
// and any consumer that needs to recognize the payload type from raw
// JSON. The constants are pinned here so the string cannot drift across
// the marshal and unmarshal helpers, and so a future audit can compare
// the canonical encoding against the literal in one place.
const (
	DecisionPayloadSchema           = "gentle-ai.review-decision-payload/v1"
	DecisionAdjudicatePayloadSchema = "gentle-ai.review-decision-adjudicate-payload/v1"
)

// DecisionPayload is the evidence bundle that authors one review/decide
// decision. The decision literal is the externally-visible contract that
// `review inspect-authority` and the journal consume; the change handler
// must reject anything other than "continue" or "stop". The optional
// Adjudication field carries the bounded adjudication payload that turns
// a `continue` decision into the next-state machine step.
//
// Field ordering is the canonical order for the marshaled JSON object:
// every JSON key appears in the canonical sorted order, and the schema
// hash is the SHA-256 of those canonical bytes. Reordering these fields
// would silently shift every persisted payload's hash, so the order is
// locked here.
type DecisionPayload struct {
	Operation    string                     `json:"operation"`
	LineageID    string                     `json:"lineage_id"`
	Decision     string                     `json:"decision"`
	Revision     string                     `json:"revision"`
	RecordedBy   string                     `json:"recorded_by"`
	RecordedAt   time.Time                  `json:"recorded_at"`
	Adjudication *DecisionAdjudicatePayload `json:"adjudication,omitempty"`
}

// DecisionAdjudicatePayload is the bounded single-shot adjudication
// outcome that authors a review/decision-adjudicate-batch invocation.
// The adjudicator string and the evidence_hashes pin the provider call
// so subsequent re-runs can detect replay.
//
// Field ordering is the canonical order for the marshaled JSON object,
// matching the convention used by DecisionPayload above.
type DecisionAdjudicatePayload struct {
	Operation      string    `json:"operation"`
	LineageID      string    `json:"lineage_id"`
	Adjudicator    string    `json:"adjudicator"`
	EvidenceHashes []string  `json:"evidence_hashes"`
	RetryCount     int       `json:"retry_count"`
	DecidedAt      time.Time `json:"decided_at"`
}

// ValidateDecisionLiteral returns an error when the literal is not the
// empty string, "continue", or "stop". The empty string is permitted so
// callers that construct a partial payload can validate incrementally
// before the final value is bound.
func ValidateDecisionLiteral(decision string) error {
	switch decision {
	case "", "continue", "stop":
		return nil
	default:
		return fmt.Errorf("review/decide: --decision must be continue or stop, got %q", decision)
	}
}

// MarshalDecisionPayload encodes the payload as compact JSON. The
// canonical encoding sorts keys alphabetically and produces no
// whitespace, which is exactly what SchemaHash consumes.
func MarshalDecisionPayload(payload DecisionPayload) ([]byte, error) {
	return canonicalJSON(payload)
}

// UnmarshalDecisionPayload decodes a previously marshaled payload. The
// decoder rejects unknown fields, mirroring the rest of the package's
// wire-format discipline, so a writer that silently adds a new field
// would be caught at parse time rather than at journal-replay time.
func UnmarshalDecisionPayload(payload []byte) (DecisionPayload, error) {
	out, err := decodeDecisionPayload(payload)
	if err != nil {
		return DecisionPayload{}, err
	}
	if err := ValidateDecisionLiteral(out.Decision); err != nil {
		return DecisionPayload{}, err
	}
	return out, nil
}

// MarshalDecisionAdjudicatePayload encodes the bounded adjudication
// payload as compact JSON with sorted keys.
func MarshalDecisionAdjudicatePayload(payload DecisionAdjudicatePayload) ([]byte, error) {
	return canonicalJSON(payload)
}

// UnmarshalDecisionAdjudicatePayload decodes a previously marshaled
// bounded adjudication payload. Unknown fields are rejected.
func UnmarshalDecisionAdjudicatePayload(payload []byte) (DecisionAdjudicatePayload, error) {
	return decodeAdjudicatePayload(payload)
}

// SchemaHash returns the deterministic SHA-256 of the canonical
// encoding of the payload. The canonical encoding is compact JSON with
// keys sorted alphabetically, which is exactly what MarshalDecisionPayload
// produces. The hash is content-only: it does not mutate the receiver.
func (p DecisionPayload) SchemaHash() [32]byte {
	return sumCanonical(p)
}

// SchemaHash returns the deterministic SHA-256 of the canonical
// encoding of the bounded adjudication payload. The canonical encoding
// is compact JSON with keys sorted alphabetically, produced by
// MarshalDecisionAdjudicatePayload.
func (p DecisionAdjudicatePayload) SchemaHash() [32]byte {
	return sumCanonical(p)
}

// decodeDecisionPayload is the shared parse path used by
// UnmarshalDecisionPayload. It rejects extra JSON values and unknown
// fields but does not validate the decision literal, so internal
// callers (e.g. partial payload construction) can skip that check.
func decodeDecisionPayload(payload []byte) (DecisionPayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var out DecisionPayload
	if err := decoder.Decode(&out); err != nil {
		return DecisionPayload{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return DecisionPayload{}, errors.New("multiple JSON values in review decision payload")
	}
	return out, nil
}

// decodeAdjudicatePayload is the shared parse path used by
// UnmarshalDecisionAdjudicatePayload. It rejects extra JSON values and
// unknown fields.
func decodeAdjudicatePayload(payload []byte) (DecisionAdjudicatePayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var out DecisionAdjudicatePayload
	if err := decoder.Decode(&out); err != nil {
		return DecisionAdjudicatePayload{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return DecisionAdjudicatePayload{}, errors.New("multiple JSON values in review decision adjudicate payload")
	}
	return out, nil
}

// canonicalJSON encodes v as compact JSON with object keys sorted
// alphabetically. It serializes via interface{} so maps preserve their
// natural alphabetical ordering; slices keep their declared order. The
// schema-hash and marshal helpers both call this so the bytes that
// round-trip through a writer are exactly the bytes the hash commits
// to.
func canonicalJSON(v interface{}) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var anyValue interface{}
	if err := json.Unmarshal(raw, &anyValue); err != nil {
		return nil, err
	}
	return json.Marshal(anyValue)
}

// sumCanonical encodes v as canonical JSON and returns the SHA-256 of
// the bytes. Encoding failures return the zero hash so the caller can
// still observe the result without panicking; in practice the only
// callers are struct types whose fields are all JSON-encodable.
func sumCanonical(v interface{}) [32]byte {
	raw, err := canonicalJSON(v)
	if err != nil {
		return [32]byte{}
	}
	return sha256.Sum256(raw)
}
