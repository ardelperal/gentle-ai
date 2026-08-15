package reviewtransaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	refPublicationsDirectory = "ref-publications"
	refPublicationsVersion   = "v1"
)

// RefPublicationRepository is the persistence seam for one publish-ref
// attempt. It is the single RAR primitive that owns the immutable record
// and the request/destination indexes; CLI commands, the transport, and
// the read-only reconciler all read and write through this seam.
type RefPublicationRepository interface {
	// Save persists an immutable record. The same request_id is replayed
	// only with the same request_digest and only while it is active;
	// every other shape is rejected.
	Save(ctx context.Context, record RefPublicationRecord) (RefPublicationRecord, error)
	// Load returns the durable record for one request_id, or
	// ErrRefPublicationUnknownRequestID if no record is persisted.
	Load(ctx context.Context, requestID string) (RefPublicationRecord, error)
	// ListByEndpointDestination returns the active records that share the
	// given endpoint identity and destination ref. Terminal records are
	// omitted so an active slot may be reused after preparation succeeds.
	ListByEndpointDestination(
		ctx context.Context,
		endpointIdentity string,
		destinationRef string,
	) ([]RefPublicationRecord, error)
	// MarkTerminal records the terminal verdict for a request_id whose
	// record is currently in prepared state. Other states are rejected.
	MarkTerminal(
		ctx context.Context,
		requestID string,
		state RefPublicationState,
		attribution RefPublicationAttribution,
		resultRef string,
	) (RefPublicationRecord, error)
}

// RarRefPublicationRepository backs RefPublicationRepository with the RAR
// authority private directory. It reuses the same Git-common-dir root, lease,
// private directory helpers, and atomic no-replace publication that the
// verification-authority and plan-authority repositories already use.
type RarRefPublicationRepository struct {
	rar  *RARAuthorityRepository
	root string
}

// OpenRefPublicationRepository opens the publish-ref persistence seam.
func OpenRefPublicationRepository(ctx context.Context, repo string) (*RarRefPublicationRepository, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rar, err := OpenRARAuthorityRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	return &RarRefPublicationRepository{
		rar:  rar,
		root: filepath.Join(rar.root, refPublicationsDirectory, refPublicationsVersion),
	}, nil
}

// RepositoryRef returns the path-free Git identity retained by the parent
// RAR authority repository. CLI commands use it to confirm the running
// repository is the same one the record was published against.
func (repository *RarRefPublicationRepository) RepositoryRef() string {
	if repository == nil {
		return ""
	}
	return repository.rar.RepositoryRef()
}

// Save persists an immutable record. The same request_id is replayed only
// with the same request_digest and only while it is active; every other
// shape is rejected before the on-disk slot is rewritten.
func (repository *RarRefPublicationRepository) Save(
	ctx context.Context,
	record RefPublicationRecord,
) (RefPublicationRecord, error) {
	if err := ctx.Err(); err != nil {
		return RefPublicationRecord{}, err
	}
	if err := record.Validate(); err != nil {
		return RefPublicationRecord{}, err
	}
	if record.State != RefPubPrepared && record.State != RefPubPushed {
		return RefPublicationRecord{}, fmt.Errorf(
			"%w: Save requires prepared or pushed state; got %q",
			ErrRefPublicationTransitionIllegal, record.State,
		)
	}
	auth, err := ParseRefPublicationAuthorization(string(record.Payload))
	if err != nil {
		return RefPublicationRecord{}, fmt.Errorf("ref publication payload: %w", err)
	}
	return repository.writeLocked(ctx, func() (RefPublicationRecord, error) {
		existing, loadErr := repository.loadLocked(record.RequestID)
		if loadErr == nil {
			if existing.RequestDigest != record.RequestDigest {
				return RefPublicationRecord{}, fmt.Errorf(
					"%w: request_id=%s", ErrRefPublicationReplayMismatch, record.RequestID,
				)
			}
			if isTerminalRefPublicationState(existing.State) {
				return RefPublicationRecord{}, fmt.Errorf(
					"%w: request_id=%s state=%q",
					ErrRefPublicationAlreadyTerminal, record.RequestID, existing.State,
				)
			}
			if !isLegalRefPublicationStateTransition(existing.State, record.State) {
				return RefPublicationRecord{}, fmt.Errorf(
					"%w: %q -> %q",
					ErrRefPublicationTransitionIllegal, existing.State, record.State,
				)
			}
		} else if !errors.Is(loadErr, errRefPublicationRecordMissing) {
			return RefPublicationRecord{}, loadErr
		}
		if record.State == RefPubPrepared {
			contested, contestErr := repository.contestedEndpointDestinationLocked(
				auth.EndpointIdentity, auth.DestinationRef, record.RequestID,
			)
			if contestErr != nil {
				return RefPublicationRecord{}, contestErr
			}
			if contested {
				return RefPublicationRecord{}, fmt.Errorf(
					"%w: endpoint=%s destination=%s",
					ErrRefPublicationAllocationContested,
					auth.EndpointIdentity, auth.DestinationRef,
				)
			}
		}
		return repository.writeRecordLocked(record, auth)
	})
}

// Load returns the durable record for one request_id, or
// ErrRefPublicationUnknownRequestID if no record is persisted.
func (repository *RarRefPublicationRepository) Load(
	ctx context.Context,
	requestID string,
) (RefPublicationRecord, error) {
	if err := ctx.Err(); err != nil {
		return RefPublicationRecord{}, err
	}
	if requestID == "" {
		return RefPublicationRecord{}, errors.New("ref publication record request_id is required")
	}
	if _, err := os.Lstat(repository.root); errors.Is(err, fs.ErrNotExist) {
		return RefPublicationRecord{}, fmt.Errorf("%w: request_id=%s", ErrRefPublicationUnknownRequestID, requestID)
	} else if err != nil {
		return RefPublicationRecord{}, err
	}
	if err := repository.ensureRoot(false); err != nil {
		return RefPublicationRecord{}, err
	}
	record, err := repository.loadLocked(requestID)
	if errors.Is(err, errRefPublicationRecordMissing) {
		return RefPublicationRecord{}, fmt.Errorf("%w: request_id=%s", ErrRefPublicationUnknownRequestID, requestID)
	}
	return record, err
}

// ListByEndpointDestination returns the active records that share the given
// endpoint identity and destination ref. Terminal records are omitted so an
// active slot may be reused after the previous attempt is finalized.
func (repository *RarRefPublicationRepository) ListByEndpointDestination(
	ctx context.Context,
	endpointIdentity string,
	destinationRef string,
) ([]RefPublicationRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if endpointIdentity == "" || destinationRef == "" {
		return nil, errors.New("ref publication endpoint_identity and destination_ref are required")
	}
	if err := repository.ensureRoot(false); err != nil {
		return nil, err
	}
	directory := repository.byEndpointDestinationDirectory(endpointIdentity, destinationRef)
	entries, err := readPrivateDirectoryEntries(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []RefPublicationRecord
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		requestID := strings.TrimSuffix(entry.Name(), ".json")
		record, loadErr := repository.loadLocked(requestID)
		if loadErr != nil || !isActiveRefPublicationState(record.State) {
			continue
		}
		auth, parseErr := ParseRefPublicationAuthorization(string(record.Payload))
		if parseErr != nil {
			continue
		}
		if auth.EndpointIdentity != endpointIdentity || auth.DestinationRef != destinationRef {
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

// MarkTerminal records the terminal verdict for a request_id whose record is
// currently in pushed state. The result is durable, canonical, and may not
// be replayed against any other request_digest.
func (repository *RarRefPublicationRepository) MarkTerminal(
	ctx context.Context,
	requestID string,
	state RefPublicationState,
	attribution RefPublicationAttribution,
	resultRef string,
) (RefPublicationRecord, error) {
	if err := ctx.Err(); err != nil {
		return RefPublicationRecord{}, err
	}
	if requestID == "" {
		return RefPublicationRecord{}, errors.New("ref publication mark terminal requires request_id")
	}
	if !isTerminalRefPublicationState(state) {
		return RefPublicationRecord{}, fmt.Errorf(
			"%w: %q", ErrRefPublicationTransitionIllegal, state,
		)
	}
	if attribution != RefPublicationAttributionProven &&
		attribution != RefPublicationAttributionUnproven {
		return RefPublicationRecord{}, errors.New("ref publication mark terminal requires a known attribution")
	}
	if state == RefPubConfirmed && attribution != RefPublicationAttributionProven {
		return RefPublicationRecord{}, errors.New("ref publication confirmed verdict requires proven attribution")
	}
	if attribution == RefPublicationAttributionProven {
		if state != RefPubConfirmed {
			return RefPublicationRecord{}, errors.New("ref publication proven attribution requires a confirmed verdict")
		}
		if resultRef == "" {
			return RefPublicationRecord{}, errors.New("ref publication proven attribution requires a result_ref")
		}
	}
	return repository.writeLocked(ctx, func() (RefPublicationRecord, error) {
		existing, loadErr := repository.loadLocked(requestID)
		if errors.Is(loadErr, errRefPublicationRecordMissing) {
			return RefPublicationRecord{}, fmt.Errorf("%w: request_id=%s", ErrRefPublicationUnknownRequestID, requestID)
		}
		if loadErr != nil {
			return RefPublicationRecord{}, loadErr
		}
		if existing.State != RefPubPushed {
			return RefPublicationRecord{}, fmt.Errorf(
				"%w: request_id=%s state=%q",
				ErrRefPublicationNotPrepared, requestID, existing.State,
			)
		}
		auth, parseErr := ParseRefPublicationAuthorization(string(existing.Payload))
		if parseErr != nil {
			return RefPublicationRecord{}, parseErr
		}
		updated := existing
		updated.State = state
		updated.Attribution = attribution
		updated.ResultRef = resultRef
		return repository.writeRecordLocked(updated, auth)
	})
}

// writeLocked holds the repository lock for the duration of body.
func (repository *RarRefPublicationRepository) writeLocked(
	ctx context.Context,
	body func() (RefPublicationRecord, error),
) (RefPublicationRecord, error) {
	if err := repository.rar.validateIdentity(ctx); err != nil {
		return RefPublicationRecord{}, err
	}
	if err := repository.ensureRoot(true); err != nil {
		return RefPublicationRecord{}, err
	}
	lock, err := acquireRARAuthorityLock(ctx, filepath.Join(repository.root, "LOCK"))
	if err != nil {
		return RefPublicationRecord{}, err
	}
	defer func() { _ = lock.release() }()
	if err := repository.rar.validateIdentity(ctx); err != nil {
		return RefPublicationRecord{}, err
	}
	return body()
}

func (repository *RarRefPublicationRepository) ensureRoot(writable bool) error {
	if err := ensureRARRepositoryRoot(
		repository.rar.identity.GitCommonDir, repository.rar.root, writable,
	); err != nil {
		return err
	}
	if err := ensurePrivateRARDirectoryTree(repository.rar.root, repository.root, writable); err != nil {
		return err
	}
	if writable {
		for _, slot := range []string{
			filepath.Join(repository.root, "objects"),
			filepath.Join(repository.root, "by-request"),
			filepath.Join(repository.root, "by-endpoint-destination"),
		} {
			if err := ensurePrivateRARDirectoryTree(repository.root, slot, writable); err != nil {
				return err
			}
		}
	}
	return nil
}

func (repository *RarRefPublicationRepository) loadLocked(requestID string) (RefPublicationRecord, error) {
	payload, err := readPrivateRARFile(repository.objectPath(requestID))
	if errors.Is(err, fs.ErrNotExist) {
		return RefPublicationRecord{}, errRefPublicationRecordMissing
	}
	if err != nil {
		return RefPublicationRecord{}, err
	}
	var record RefPublicationRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return RefPublicationRecord{}, fmt.Errorf("parse ref publication record: %w", err)
	}
	if err := record.Validate(); err != nil {
		return RefPublicationRecord{}, fmt.Errorf("%w: %v", ErrRARAuthorityCorrupt, err)
	}
	return record, nil
}

func (repository *RarRefPublicationRepository) writeRecordLocked(
	record RefPublicationRecord,
	auth RefPublicationAuthorization,
) (RefPublicationRecord, error) {
	if record.State == RefPubPrepared {
		record.RequestDigest = RefPublicationAuthorizationDigest(auth)
		auth.RequestDigest = record.RequestDigest
		record.Payload = []byte(auth.Payload())
	}
	if err := record.Validate(); err != nil {
		return RefPublicationRecord{}, err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return RefPublicationRecord{}, fmt.Errorf("encode ref publication record: %w", err)
	}
	for _, slot := range []string{
		filepath.Join(repository.root, "objects"),
		filepath.Join(repository.root, "by-request"),
	} {
		if err := ensurePrivateRARDirectoryTree(repository.root, slot, true); err != nil {
			return RefPublicationRecord{}, err
		}
	}
	if err := writePrivateRecordFile(repository.objectPath(record.RequestID), payload); err != nil {
		return RefPublicationRecord{}, err
	}
	if err := writePrivateRecordFile(
		repository.byRequestPath(record.RequestID), payload,
	); err != nil {
		return RefPublicationRecord{}, err
	}
	if isActiveRefPublicationState(record.State) {
		indexPath := repository.byEndpointDestinationPath(
			auth.EndpointIdentity, auth.DestinationRef, record.RequestID,
		)
		if err := ensurePrivateRARDirectoryTree(repository.root, filepath.Dir(indexPath), true); err != nil {
			return record, err
		}
		if err := writePrivateRecordFile(indexPath, payload); err != nil {
			return record, err
		}
	}
	return record, nil
}

// writePrivateRecordFile replaces the existing private record at path with the
// given payload, atomically. The RAR no-replace primitive refuses to overwrite
// a published slot, so the stateful record file uses replace semantics: a
// state transition rewrites the same canonical path with the new payload.
// The caller is responsible for ensuring the parent directory exists.
func writePrivateRecordFile(path string, payload []byte) error {
	if len(payload) == 0 || len(payload) > refPublicationRecordMaxBytes {
		return errors.New("ref publication record payload size is invalid")
	}
	dir := filepath.Dir(path)
	if err := validatePrivateRARDirectory(dir); err != nil {
		return err
	}
	temp, err := createPrivateRARTempFile(dir)
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := validatePrivateRARFile(tempPath); err != nil {
		return err
	}
	if err := replaceFileAtomic(tempPath, path); err != nil {
		return err
	}
	if err := validatePrivateRARFile(path); err != nil {
		return err
	}
	return SyncReviewDirectory(dir)
}

func (repository *RarRefPublicationRepository) contestedEndpointDestinationLocked(
	endpointIdentity, destinationRef, requestID string,
) (bool, error) {
	records, err := repository.listByEndpointDestinationLocked(endpointIdentity, destinationRef)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if record.RequestID != requestID {
			return true, nil
		}
	}
	return false, nil
}

func (repository *RarRefPublicationRepository) listByEndpointDestinationLocked(
	endpointIdentity, destinationRef string,
) ([]RefPublicationRecord, error) {
	directory := repository.byEndpointDestinationDirectory(endpointIdentity, destinationRef)
	entries, err := readPrivateDirectoryEntries(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []RefPublicationRecord
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		requestID := strings.TrimSuffix(entry.Name(), ".json")
		record, loadErr := repository.loadLocked(requestID)
		if loadErr != nil || !isActiveRefPublicationState(record.State) {
			continue
		}
		auth, parseErr := ParseRefPublicationAuthorization(string(record.Payload))
		if parseErr != nil {
			continue
		}
		if auth.EndpointIdentity != endpointIdentity || auth.DestinationRef != destinationRef {
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

func (repository *RarRefPublicationRepository) objectPath(requestID string) string {
	return filepath.Join(repository.root, "objects", requestID+".json")
}

func (repository *RarRefPublicationRepository) byRequestPath(requestID string) string {
	return filepath.Join(repository.root, "by-request", requestID+".json")
}

func (repository *RarRefPublicationRepository) byEndpointDestinationDirectory(
	endpointIdentity, destinationRef string,
) string {
	return filepath.Join(
		repository.root,
		"by-endpoint-destination",
		hashPathComponent(endpointIdentity),
		pathSafeRef(destinationRef),
	)
}

func (repository *RarRefPublicationRepository) byEndpointDestinationPath(
	endpointIdentity, destinationRef, requestID string,
) string {
	return filepath.Join(
		repository.byEndpointDestinationDirectory(endpointIdentity, destinationRef),
		requestID+".json",
	)
}

func readPrivateDirectoryEntries(path string) ([]fs.DirEntry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("ref publication private path %q is not a directory", path)
	}
	return os.ReadDir(path)
}

// pathSafeRef encodes a refs/heads/* dotted branch-style name as a single
// path segment. The character set is restricted to ASCII, so URL-encoding
// the slash is enough to keep the component portable across Windows and
// POSIX without colliding with any legal ref name.
func pathSafeRef(ref string) string {
	return strings.ReplaceAll(ref, "/", "%2F")
}
