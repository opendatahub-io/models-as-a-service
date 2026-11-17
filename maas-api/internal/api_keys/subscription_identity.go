package api_keys

// Subscription identity persistence deliberately separates the stable database
// identity from Kubernetes object identity. The database UUID identifies one
// subscription incarnation across every projected cluster copy, while a
// Kubernetes UID is only that cluster's observation fingerprint. Keeping the
// two concepts distinct prevents same-name replacement in one cluster from
// selecting or revoking keys that belong to another incarnation.
//
// Observer sessions combine the observer fence, per-name locks, and row locks
// in one transaction. Cleanup is therefore a consensus operation: it waits for
// fresh observers and a barrier proving that none still report the identity
// present. The observer and observation state exists so deletion never has to
// trust namespace and name alone.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var ErrSubscriptionIdentityPending = errors.New("subscription identity rotation is pending")

var ErrSubscriptionIdentityAmbiguous = errors.New("subscription identity is ambiguous")

type SubscriptionCleanupResult string

const (
	SubscriptionCleanupRetained          SubscriptionCleanupResult = "retained"
	SubscriptionCleanupRetainedPolicy    SubscriptionCleanupResult = "retained_policy"
	SubscriptionCleanupBlockedEmpty      SubscriptionCleanupResult = "blocked_empty"
	SubscriptionCleanupBlockedStale      SubscriptionCleanupResult = "blocked_stale"
	SubscriptionCleanupBlockedBarrier    SubscriptionCleanupResult = "blocked_barrier"
	SubscriptionCleanupBlockedUnverified SubscriptionCleanupResult = "blocked_unverified"
	SubscriptionCleanupCleaned           SubscriptionCleanupResult = "cleaned"
	SubscriptionCleanupNotFound          SubscriptionCleanupResult = "not_found"
)

// SubscriptionObservation is one observer's retained view of a subscription
// incarnation. Absence retains the last object UID so same-name replacement can
// be distinguished from an observer restart.
type SubscriptionObservation struct {
	SubscriptionID string
	Namespace      string
	Name           string
	ObjectUID      string
	Absent         bool
}

// ResolveSubscriptionIdentityRequest describes one authoritative local object
// observation. StatusIdentityID may be empty for a new or status-less copy.
type ResolveSubscriptionIdentityRequest struct {
	Namespace        string
	Name             string
	ObjectUID        string
	StatusIdentityID string
	Settle           time.Duration
}

type SubscriptionAbsenceRequest struct {
	Namespace              string
	Name                   string
	ObjectUID              string
	SubscriptionIdentityID string
	RetainAPIKeys          bool
}

// SubscriptionIdentityStore opens transaction-scoped observer sessions. The
// session holds a per-observer advisory lock while the caller performs an
// uncached Kubernetes list.
type SubscriptionIdentityStore interface {
	BeginSubscriptionObservation(ctx context.Context, observerID, tenant string) (SubscriptionObservationSession, error)
}

// SubscriptionObservationSession serializes writers for one observer and owns
// all identity mutations in one database transaction.
type SubscriptionObservationSession interface {
	CurrentObservations(ctx context.Context) ([]SubscriptionObservation, error)
	Heartbeat(ctx context.Context) error
	RetireStaleObservers(ctx context.Context, freshWithin, staleAfter time.Duration) ([]SubscriptionObserverRetirement, error)
	ResolveSubscriptionIdentity(ctx context.Context, request ResolveSubscriptionIdentityRequest) (string, error)
	BackfillSubscriptionKeys(ctx context.Context, identityID, namespace, name string) (int, error)
	MarkAbsent(ctx context.Context, subscriptionID string) error
	RecordAbsenceAndCleanup(ctx context.Context, request SubscriptionAbsenceRequest, settle, maxObservationAge time.Duration) (SubscriptionCleanupResult, int, error)
	Commit() error
	Rollback() error
}

type SubscriptionObserverRetirement struct {
	ObserverID     string
	Result         SubscriptionCleanupResult
	RevokedAPIKeys int
}

type postgresObservationSession struct {
	tx         *sql.Tx
	store      *PostgresStore
	observerID string
	tenant     string
}

var _ SubscriptionIdentityStore = (*PostgresStore)(nil)

//nolint:ireturn // session interface by design
func (s *PostgresStore) BeginSubscriptionObservation(ctx context.Context, observerID, tenant string) (SubscriptionObservationSession, error) {
	if _, err := uuid.Parse(observerID); err != nil {
		return nil, fmt.Errorf("observer ID must be a UUID: %w", err)
	}
	if tenant != s.tenantName {
		return nil, fmt.Errorf("%w: attempted to observe tenant %q but store is scoped to %q", ErrTenantMismatch, tenant, s.tenantName)
	}

	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin subscription observation transaction: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", observerID); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("failed to acquire subscription observer fence: %w", err)
	}
	return &postgresObservationSession{tx: tx, store: s, observerID: observerID, tenant: tenant}, nil
}

func (session *postgresObservationSession) CurrentObservations(ctx context.Context) ([]SubscriptionObservation, error) {
	rows, err := session.tx.QueryContext(ctx, `
		SELECT identity.id, identity.namespace, identity.name, observation.object_uid, observation.absent_at
		FROM subscription_observations observation
		JOIN subscription_identities identity ON identity.id = observation.subscription_id
		WHERE observation.observer_id = $1
		  AND identity.deleted_at IS NULL
	`, session.observerID)
	if err != nil {
		return nil, fmt.Errorf("failed to list current subscription observations: %w", err)
	}
	defer rows.Close()

	observations := make([]SubscriptionObservation, 0)
	for rows.Next() {
		var observation SubscriptionObservation
		var absentAt sql.NullTime
		if err := rows.Scan(&observation.SubscriptionID, &observation.Namespace, &observation.Name, &observation.ObjectUID, &absentAt); err != nil {
			return nil, fmt.Errorf("failed to scan subscription observation: %w", err)
		}
		observation.Absent = absentAt.Valid
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate subscription observations: %w", err)
	}
	return observations, nil
}

func (session *postgresObservationSession) Heartbeat(ctx context.Context) error {
	var retiredAt sql.NullTime
	err := session.tx.QueryRowContext(ctx, `
		INSERT INTO subscription_observers (observer_id, tenant, observed_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (observer_id) DO UPDATE
		SET observed_at = NOW()
		WHERE subscription_observers.retired_at IS NULL
		  AND subscription_observers.tenant = EXCLUDED.tenant
		RETURNING retired_at
	`, session.observerID, session.tenant).Scan(&retiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		var existingTenant string
		var existingRetiredAt sql.NullTime
		lookupErr := session.tx.QueryRowContext(ctx, `
			SELECT tenant, retired_at
			FROM subscription_observers
			WHERE observer_id = $1
		`, session.observerID).Scan(&existingTenant, &existingRetiredAt)
		if lookupErr != nil {
			return fmt.Errorf("failed to inspect subscription observer heartbeat: %w", lookupErr)
		}
		if existingTenant != session.tenant {
			return fmt.Errorf("%w: observer %s is registered to tenant %q", ErrTenantMismatch, session.observerID, existingTenant)
		}
		if existingRetiredAt.Valid {
			return ErrObserverRetired
		}
		return errors.New("subscription observer heartbeat unexpectedly matched no state")
	}
	if err != nil {
		return fmt.Errorf("failed to refresh subscription observer heartbeat: %w", err)
	}
	if retiredAt.Valid {
		return ErrObserverRetired
	}
	return nil
}

func (session *postgresObservationSession) ResolveSubscriptionIdentity(ctx context.Context, request ResolveSubscriptionIdentityRequest) (string, error) {
	if request.Namespace == "" || request.Name == "" || request.ObjectUID == "" {
		return "", errors.New("subscription namespace, name, and object UID are required")
	}
	if err := session.lockSubscriptionName(ctx, request.Name); err != nil {
		return "", err
	}

	var identityID string
	var rotationPendingAt sql.NullTime
	err := session.tx.QueryRowContext(ctx, `
		SELECT id, rotation_pending_at
		FROM subscription_identities
		WHERE tenant = $1 AND namespace = $2 AND name = $3 AND deleted_at IS NULL
		FOR UPDATE
	`, session.tenant, request.Namespace, request.Name).Scan(&identityID, &rotationPendingAt)
	if errors.Is(err, sql.ErrNoRows) {
		identityID, err := session.insertObservation(ctx, request.Namespace, request.Name, request.ObjectUID)
		if err != nil {
			return "", err
		}
		return identityID, nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to lock subscription identity: %w", err)
	}
	if rotationPendingAt.Valid {
		if err := session.markSupersededObservationAbsent(ctx, identityID, request.ObjectUID); err != nil {
			return "", err
		}
		newIdentityID, err := session.recoverRotatedIdentity(ctx, request, identityID)
		if err != nil {
			return "", err
		}
		if newIdentityID != "" {
			return newIdentityID, nil
		}
		return "", ErrSubscriptionIdentityPending
	}

	priorID, err := session.identityForObjectUID(ctx, request)
	if err != nil {
		return "", err
	}
	if priorID != "" && priorID != identityID {
		if err := session.rotateIdentity(ctx, identityID); err != nil {
			return "", err
		}
		return "", ErrSubscriptionIdentityPending
	}

	if request.StatusIdentityID != "" {
		if request.StatusIdentityID != identityID {
			if err := session.rotateIdentity(ctx, identityID); err != nil {
				return "", err
			}
			return "", ErrSubscriptionIdentityMismatch
		}
		if err := session.upsertObservation(ctx, identityID, request.ObjectUID); err != nil {
			return "", err
		}
		return identityID, nil
	}

	if priorID == identityID {
		if err := session.upsertObservation(ctx, identityID, request.ObjectUID); err != nil {
			return "", err
		}
		return identityID, nil
	}

	priorObjectUID, _, err := session.priorObservationForName(ctx, request)
	if err != nil {
		return "", err
	}
	if priorObjectUID != "" && priorObjectUID != request.ObjectUID {
		if err := session.rotateIdentity(ctx, identityID); err != nil {
			return "", err
		}
		return "", ErrSubscriptionIdentityPending
	}

	var observedElsewhere bool
	err = session.tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM subscription_observations observation
			JOIN subscription_observers observer ON observer.observer_id = observation.observer_id
			WHERE observation.subscription_id = $1
			  AND observation.absent_at IS NULL
			  AND observer.retired_at IS NULL
			  AND observation.observer_id <> $2
			  AND observer.tenant = $3
		)
		`, identityID, session.observerID, session.tenant).Scan(&observedElsewhere)
	if err != nil {
		return "", fmt.Errorf("failed to check peer subscription observations: %w", err)
	}
	if !observedElsewhere {
		if err := session.MarkAbsent(ctx, identityID); err != nil {
			return "", err
		}
		return "", ErrSubscriptionIdentityPending
	}

	if err := session.upsertObservation(ctx, identityID, request.ObjectUID); err != nil {
		return "", err
	}
	return identityID, nil
}

// lockSubscriptionName serializes identity resolution for one tenant and
// subscription name. The tenant length is included in the advisory-lock key so
// tenant/name concatenations cannot collide with a different split.
func (session *postgresObservationSession) lockSubscriptionName(ctx context.Context, name string) error {
	lockKey := fmt.Sprintf("%d:%s%s", len(session.tenant), session.tenant, name)
	if _, err := session.tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", lockKey); err != nil {
		return fmt.Errorf("failed to acquire subscription identity lock: %w", err)
	}
	return nil
}

func (session *postgresObservationSession) BackfillSubscriptionKeys(
	ctx context.Context,
	identityID, namespace, name string,
) (int, error) {
	if namespace == "" || name == "" {
		return 0, errors.New("subscription namespace and name are required")
	}
	if _, err := uuid.Parse(identityID); err != nil {
		return 0, fmt.Errorf("%w: invalid UUID", ErrSubscriptionIdentityRequired)
	}
	if err := session.lockSubscriptionName(ctx, name); err != nil {
		return 0, err
	}

	var activeIdentityCount int
	if err := session.tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM subscription_identities
		WHERE tenant = $1
		  AND name = $2
		  AND deleted_at IS NULL
	`, session.tenant, name).Scan(&activeIdentityCount); err != nil {
		return 0, fmt.Errorf("failed to count active subscription identities for key backfill: %w", err)
	}
	if activeIdentityCount != 1 {
		return 0, fmt.Errorf("%w: %d active identities for subscription %q", ErrSubscriptionIdentityAmbiguous, activeIdentityCount, name)
	}

	var selectedIdentityID, selectedNamespace string
	err := session.tx.QueryRowContext(ctx, `
		SELECT id, namespace
		FROM subscription_identities
		WHERE tenant = $1
		  AND name = $2
		  AND deleted_at IS NULL
		  AND rotation_pending_at IS NULL
		FOR UPDATE
	`, session.tenant, name).Scan(&selectedIdentityID, &selectedNamespace)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrSubscriptionIdentityRequired
	}
	if err != nil {
		return 0, fmt.Errorf("failed to lock subscription identity for key backfill: %w", err)
	}
	if selectedIdentityID != identityID || selectedNamespace != namespace {
		return 0, fmt.Errorf("%w: identity does not match %s/%s", ErrSubscriptionIdentityAmbiguous, namespace, name)
	}

	result, err := session.tx.ExecContext(ctx, `
		UPDATE api_keys
		SET subscription_id = $1,
		    subscription_namespace = $2
		WHERE tenant = $3
		  AND subscription = $4
		  AND subscription_id IS NULL
		  AND subscription_namespace IS NULL
		  AND status = 'active'
		  AND deleted_at IS NULL
	`, identityID, namespace, session.tenant, name)
	if err != nil {
		return 0, fmt.Errorf("failed to backfill API-key subscription identities: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to count backfilled API-key subscription identities: %w", err)
	}

	var unbound bool
	if err := session.tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM api_keys
			WHERE tenant = $1
			  AND subscription = $2
			  AND status = 'active'
			  AND deleted_at IS NULL
			  AND (
				subscription_id IS NULL
				OR subscription_namespace IS NULL
				OR subscription_id <> $3::uuid
				OR subscription_namespace <> $4
			  )
		)
	`, session.tenant, name, identityID, namespace).Scan(&unbound); err != nil {
		return 0, fmt.Errorf("failed to verify API-key subscription identity bindings: %w", err)
	}
	if unbound {
		return 0, fmt.Errorf("%w: active key has an incorrect binding for subscription %q", ErrSubscriptionIdentityAmbiguous, name)
	}
	return int(rows), nil
}

func (session *postgresObservationSession) recoverRotatedIdentity(
	ctx context.Context,
	request ResolveSubscriptionIdentityRequest,
	identityID string,
) (string, error) {
	var observationCount int
	var presentOrSettling bool
	err := session.tx.QueryRowContext(ctx, `
		SELECT COUNT(*),
			EXISTS (
				SELECT 1
				FROM subscription_observations observation
				JOIN subscription_observers observer ON observer.observer_id = observation.observer_id
				WHERE observation.subscription_id = $1
				  AND observer.retired_at IS NULL
				  AND observer.tenant = $2
				  AND (
					observation.absent_at IS NULL
					OR observation.absent_at > NOW() - ($3 * INTERVAL '1 second')
				  )
			)
		FROM subscription_observations observation
		JOIN subscription_observers observer ON observer.observer_id = observation.observer_id
		WHERE observation.subscription_id = $1
		  AND observer.retired_at IS NULL
		  AND observer.tenant = $2
	`, identityID, session.tenant, settleSeconds(request.Settle)).Scan(&observationCount, &presentOrSettling)
	if err != nil {
		return "", fmt.Errorf("failed to evaluate rotated subscription identity: %w", err)
	}
	if observationCount > 0 && presentOrSettling {
		return "", nil
	}

	var selectedID string
	err = session.tx.QueryRowContext(ctx, `
		SELECT id
		FROM subscription_identities
		WHERE tenant = $1 AND namespace = $2 AND name = $3 AND deleted_at IS NULL
		FOR UPDATE
	`, session.tenant, request.Namespace, request.Name).Scan(&selectedID)
	if errors.Is(err, sql.ErrNoRows) {
		return session.insertObservation(ctx, request.Namespace, request.Name, request.ObjectUID)
	}
	if err != nil {
		return "", fmt.Errorf("failed to lock rotated subscription identity: %w", err)
	}
	if _, err := session.tx.ExecContext(ctx, `
		UPDATE subscription_identities
		SET deleted_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL
	`, selectedID); err != nil {
		return "", fmt.Errorf("failed to tombstone rotated subscription identity: %w", err)
	}
	return session.insertObservation(ctx, request.Namespace, request.Name, request.ObjectUID)
}

func (session *postgresObservationSession) markSupersededObservationAbsent(
	ctx context.Context,
	identityID, objectUID string,
) error {
	_, err := session.tx.ExecContext(ctx, `
		UPDATE subscription_observations
		SET absent_at = NOW()
		WHERE subscription_id = $1
		  AND observer_id = $2
		  AND absent_at IS NULL
		  AND object_uid <> $3
	`, identityID, session.observerID, objectUID)
	if err != nil {
		return fmt.Errorf("failed to mark superseded subscription observation absent: %w", err)
	}
	return nil
}

func settleSeconds(settle time.Duration) int {
	if settle > 0 {
		return int(settle.Seconds())
	}
	return 30
}

func (session *postgresObservationSession) insertObservation(ctx context.Context, namespace, name, objectUID string) (string, error) {
	var identityID string
	err := session.tx.QueryRowContext(ctx, `
		INSERT INTO subscription_identities (tenant, namespace, name)
		VALUES ($1, $2, $3)
		RETURNING id
	`, session.tenant, namespace, name).Scan(&identityID)
	if err != nil {
		return "", fmt.Errorf("failed to allocate subscription identity: %w", err)
	}
	if err := session.upsertObservation(ctx, identityID, objectUID); err != nil {
		return "", err
	}
	return identityID, nil
}

func (session *postgresObservationSession) upsertObservation(ctx context.Context, identityID, objectUID string) error {
	_, err := session.tx.ExecContext(ctx, `
		INSERT INTO subscription_observations (subscription_id, observer_id, object_uid, absent_at, observed_at)
		VALUES ($1, $2, $3, NULL, NOW())
		ON CONFLICT (subscription_id, observer_id) DO UPDATE
		SET object_uid = EXCLUDED.object_uid,
		    absent_at = NULL,
		    observed_at = NOW()
	`, identityID, session.observerID, objectUID)
	if err != nil {
		return fmt.Errorf("failed to upsert subscription observation: %w", err)
	}
	return nil
}

func (session *postgresObservationSession) identityForObjectUID(ctx context.Context, request ResolveSubscriptionIdentityRequest) (string, error) {
	var identityID string
	err := session.tx.QueryRowContext(ctx, `
		SELECT observation.subscription_id
		FROM subscription_observations observation
		JOIN subscription_identities identity ON identity.id = observation.subscription_id
		WHERE observation.observer_id = $1
		  AND observation.object_uid = $2
		  AND identity.tenant = $3
		  AND identity.namespace = $4
		  AND identity.name = $5
		  AND identity.deleted_at IS NULL
		LIMIT 1
	`, session.observerID, request.ObjectUID, session.tenant, request.Namespace, request.Name).Scan(&identityID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to recover subscription identity from object UID: %w", err)
	}
	return identityID, nil
}

func (session *postgresObservationSession) priorObservationForName(ctx context.Context, request ResolveSubscriptionIdentityRequest) (string, string, error) {
	var objectUID, identityID string
	err := session.tx.QueryRowContext(ctx, `
		SELECT observation.object_uid, observation.subscription_id
		FROM subscription_observations observation
		JOIN subscription_identities identity ON identity.id = observation.subscription_id
		WHERE observation.observer_id = $1
		  AND identity.tenant = $2
		  AND identity.namespace = $3
		  AND identity.name = $4
		  AND identity.deleted_at IS NULL
		ORDER BY observation.observed_at DESC
		LIMIT 1
	`, session.observerID, session.tenant, request.Namespace, request.Name).Scan(&objectUID, &identityID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("failed to read prior subscription observation: %w", err)
	}
	return objectUID, identityID, nil
}

func (session *postgresObservationSession) rotateIdentity(ctx context.Context, identityID string) error {
	if _, err := session.tx.ExecContext(ctx, `
		UPDATE subscription_identities
		SET rotation_pending_at = COALESCE(rotation_pending_at, NOW())
		WHERE id = $1 AND deleted_at IS NULL
	`, identityID); err != nil {
		return fmt.Errorf("failed to mark subscription identity rotation: %w", err)
	}
	if _, err := session.tx.ExecContext(ctx, `
		UPDATE api_keys
		SET deleted_at = NOW(),
		    status = CASE WHEN status = 'active' THEN 'revoked' ELSE status END
		WHERE subscription_id = $1 AND deleted_at IS NULL
	`, identityID); err != nil {
		return fmt.Errorf("failed to revoke keys for rotated subscription identity: %w", err)
	}
	return session.MarkAbsent(ctx, identityID)
}

func (session *postgresObservationSession) MarkAbsent(ctx context.Context, subscriptionID string) error {
	if _, err := uuid.Parse(subscriptionID); err != nil {
		return fmt.Errorf("subscription identity ID must be a UUID: %w", err)
	}
	if _, err := session.tx.ExecContext(ctx, `
		UPDATE subscription_observations
		SET absent_at = COALESCE(absent_at, NOW()),
		    observed_at = NOW()
		WHERE subscription_id = $1 AND observer_id = $2
	`, subscriptionID, session.observerID); err != nil {
		return fmt.Errorf("failed to mark subscription observation absent: %w", err)
	}
	return nil
}

// RecordAbsenceAndCleanup first records this observer's absence, then revokes
// keys only after the settle window and active-observer barrier prove that no
// projected copy still observes the subscription incarnation. Conflict results
// are intentional: they retain keys until identity-aware consensus is safe.
func (session *postgresObservationSession) RecordAbsenceAndCleanup(
	ctx context.Context,
	request SubscriptionAbsenceRequest,
	settle, maxObservationAge time.Duration,
) (SubscriptionCleanupResult, int, error) {
	if err := validateSubscriptionAbsence(request, settle, maxObservationAge); err != nil {
		return "", 0, err
	}

	if _, err := session.tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", session.tenant+":"+request.Namespace+":"+request.Name); err != nil {
		return "", 0, fmt.Errorf("failed to acquire subscription identity lock: %w", err)
	}

	identityID, result, err := session.lockIdentityForAbsence(ctx, request)
	if err != nil {
		return "", 0, err
	}
	if result != "" {
		return result, 0, nil
	}

	if err := session.MarkAbsent(ctx, identityID); err != nil {
		return "", 0, err
	}
	if request.RetainAPIKeys {
		return SubscriptionCleanupRetainedPolicy, 0, nil
	}

	return session.cleanupAfterAbsence(ctx, identityID, settle, maxObservationAge)
}

func validateSubscriptionAbsence(request SubscriptionAbsenceRequest, settle, maxObservationAge time.Duration) error {
	if request.Namespace == "" || request.Name == "" || request.ObjectUID == "" {
		return errors.New("subscription namespace, name, and object UID are required")
	}
	if request.SubscriptionIdentityID != "" {
		if _, err := uuid.Parse(request.SubscriptionIdentityID); err != nil {
			return fmt.Errorf("%w: invalid UUID", ErrSubscriptionIdentityMismatch)
		}
	}
	if settle <= 0 || maxObservationAge <= 0 {
		return errors.New("settle interval and maximum observation age must be positive")
	}
	return nil
}

func (session *postgresObservationSession) lockIdentityForAbsence(
	ctx context.Context,
	request SubscriptionAbsenceRequest,
) (string, SubscriptionCleanupResult, error) {
	identityID := request.SubscriptionIdentityID
	if identityID == "" {
		var err error
		var result SubscriptionCleanupResult
		identityID, result, err = session.recoverIdentityFromObservation(ctx, request)
		if err != nil {
			return "", "", err
		}
		if result != "" {
			return "", result, nil
		}
	}

	var rotationPendingAt, deletedAt sql.NullTime
	err := session.tx.QueryRowContext(ctx, `
		SELECT id, rotation_pending_at, deleted_at
		FROM subscription_identities
		WHERE id = $1 AND tenant = $2 AND namespace = $3 AND name = $4
		FOR UPDATE
	`, identityID, session.tenant, request.Namespace, request.Name).Scan(&identityID, &rotationPendingAt, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", SubscriptionCleanupNotFound, nil
	}
	if err != nil {
		return "", "", fmt.Errorf("failed to lock subscription identity for cleanup: %w", err)
	}
	if deletedAt.Valid {
		return "", SubscriptionCleanupCleaned, nil
	}

	var observedObjectUID string
	var absentAt sql.NullTime
	err = session.tx.QueryRowContext(ctx, `
		SELECT object_uid, absent_at
		FROM subscription_observations
		WHERE subscription_id = $1 AND observer_id = $2
	`, identityID, session.observerID).Scan(&observedObjectUID, &absentAt)
	if errors.Is(err, sql.ErrNoRows) {
		if err := session.insertAbsentObservation(ctx, identityID, request.ObjectUID); err != nil {
			return "", "", err
		}
		return identityID, "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("failed to read subscription observation for cleanup: %w", err)
	}
	if !absentAt.Valid && observedObjectUID != request.ObjectUID {
		return "", "", ErrSubscriptionIdentityMismatch
	}
	return identityID, "", nil
}

func (session *postgresObservationSession) recoverIdentityFromObservation(
	ctx context.Context,
	request SubscriptionAbsenceRequest,
) (string, SubscriptionCleanupResult, error) {
	var identityID string
	err := session.tx.QueryRowContext(ctx, `
		SELECT identity.id
		FROM subscription_observations observation
		JOIN subscription_identities identity ON identity.id = observation.subscription_id
		WHERE observation.observer_id = $1
		  AND observation.object_uid = $2
		  AND identity.tenant = $3
		  AND identity.namespace = $4
		  AND identity.name = $5
		  AND identity.deleted_at IS NULL
		LIMIT 1
	`, session.observerID, request.ObjectUID, session.tenant, request.Namespace, request.Name).Scan(&identityID)
	if errors.Is(err, sql.ErrNoRows) {
		var tombstonedIdentityID string
		tombstoneErr := session.tx.QueryRowContext(ctx, `
			SELECT identity.id
			FROM subscription_observations observation
			JOIN subscription_identities identity ON identity.id = observation.subscription_id
			WHERE observation.observer_id = $1
			  AND observation.object_uid = $2
			  AND identity.tenant = $3
			  AND identity.namespace = $4
			  AND identity.name = $5
			  AND identity.deleted_at IS NOT NULL
			LIMIT 1
		`, session.observerID, request.ObjectUID, session.tenant, request.Namespace, request.Name).Scan(&tombstonedIdentityID)
		if tombstoneErr == nil {
			return "", SubscriptionCleanupCleaned, nil
		}
		if !errors.Is(tombstoneErr, sql.ErrNoRows) {
			return "", "", fmt.Errorf("failed to recover tombstoned subscription identity for absence: %w", tombstoneErr)
		}

		var nameIdentityID string
		err = session.tx.QueryRowContext(ctx, `
			SELECT id
			FROM subscription_identities
			WHERE tenant = $1 AND namespace = $2 AND name = $3 AND deleted_at IS NULL
			FOR UPDATE
		`, session.tenant, request.Namespace, request.Name).Scan(&nameIdentityID)
		if errors.Is(err, sql.ErrNoRows) {
			return "", SubscriptionCleanupCleaned, nil
		}
		if err != nil {
			return "", "", fmt.Errorf("failed to lock subscription identity by name for cleanup: %w", err)
		}
		return "", SubscriptionCleanupBlockedUnverified, nil
	}
	if err != nil {
		return "", "", fmt.Errorf("failed to recover subscription identity for absence: %w", err)
	}
	return identityID, "", nil
}

func (session *postgresObservationSession) cleanupAfterAbsence(
	ctx context.Context,
	identityID string,
	settle, maxObservationAge time.Duration,
) (SubscriptionCleanupResult, int, error) {
	var observerCount int
	if err := session.tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM subscription_observers
		WHERE tenant = $1 AND retired_at IS NULL
	`, session.tenant).Scan(&observerCount); err != nil {
		return "", 0, fmt.Errorf("failed to count active subscription observers: %w", err)
	}
	if observerCount == 0 {
		return SubscriptionCleanupBlockedEmpty, 0, nil
	}

	var stale bool
	err := session.tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM subscription_observers
			WHERE tenant = $1 AND retired_at IS NULL AND observed_at < NOW() - ($2 * INTERVAL '1 second')
		)
	`, session.tenant, int(maxObservationAge.Seconds())).Scan(&stale)
	if err != nil {
		return "", 0, fmt.Errorf("failed to check subscription observer freshness: %w", err)
	}
	if stale {
		return SubscriptionCleanupBlockedStale, 0, nil
	}

	var present bool
	err = session.tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM subscription_observations observation
			JOIN subscription_observers observer ON observer.observer_id = observation.observer_id
			WHERE observation.subscription_id = $1
			  AND observation.absent_at IS NULL
			  AND observer.retired_at IS NULL
			  AND observer.tenant = $2
		)
	`, identityID, session.tenant).Scan(&present)
	if err != nil {
		return "", 0, fmt.Errorf("failed to check remaining subscription observations: %w", err)
	}
	if present {
		return SubscriptionCleanupRetained, 0, nil
	}

	var barrier sql.NullTime
	err = session.tx.QueryRowContext(ctx, `
		SELECT MAX(observation.absent_at + ($2 * INTERVAL '1 second'))
		FROM subscription_observations observation
		JOIN subscription_observers observer ON observer.observer_id = observation.observer_id
		WHERE observation.subscription_id = $1
		  AND observation.absent_at IS NOT NULL
		  AND observer.retired_at IS NULL
		  AND observer.tenant = $3
	`, identityID, int(settle.Seconds()), session.tenant).Scan(&barrier)
	if err != nil {
		return "", 0, fmt.Errorf("failed to derive subscription cleanup barrier: %w", err)
	}
	if !barrier.Valid {
		return SubscriptionCleanupBlockedEmpty, 0, nil
	}

	var barrierNotReached, heartbeatPredatesBarrier bool
	err = session.tx.QueryRowContext(ctx, `
		SELECT NOW() < $1,
			EXISTS (
				SELECT 1 FROM subscription_observers
				WHERE tenant = $2 AND retired_at IS NULL AND observed_at < $1
			)
	`, barrier.Time, session.tenant).Scan(&barrierNotReached, &heartbeatPredatesBarrier)
	if err != nil {
		return "", 0, fmt.Errorf("failed to evaluate subscription cleanup barrier: %w", err)
	}
	if barrierNotReached || heartbeatPredatesBarrier {
		return SubscriptionCleanupBlockedBarrier, 0, nil
	}

	revocation, err := session.tx.ExecContext(ctx, `
		UPDATE api_keys
		SET deleted_at = NOW(),
		    status = CASE WHEN status = 'active' THEN 'revoked' ELSE status END
		WHERE subscription_id = $1 AND deleted_at IS NULL
	`, identityID)
	if err != nil {
		return "", 0, fmt.Errorf("failed to revoke keys for final subscription copy: %w", err)
	}
	rows, err := revocation.RowsAffected()
	if err != nil {
		return "", 0, fmt.Errorf("failed to count revoked keys for final subscription copy: %w", err)
	}
	if _, err := session.tx.ExecContext(ctx, `
		UPDATE subscription_identities
		SET deleted_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL
	`, identityID); err != nil {
		return "", 0, fmt.Errorf("failed to tombstone subscription identity: %w", err)
	}
	return SubscriptionCleanupCleaned, int(rows), nil
}

// RetireStaleObservers removes observers that cannot have run for longer than
// the retirement threshold, but only while another fresh observer proves that
// observation remains active for the tenant. Retirement and conditional key
// cleanup share this transaction: a stale observer can no longer block cleanup,
// yet an identity remains live if any active observer still reports it present.
// For an identity with no active present observation, the stale threshold is
// the safety barrier; this avoids requiring a fresh observer to have ever seen
// an incarnation that may have existed only on the removed cluster.
func (session *postgresObservationSession) RetireStaleObservers(
	ctx context.Context,
	freshWithin, staleAfter time.Duration,
) ([]SubscriptionObserverRetirement, error) {
	if freshWithin <= 0 || staleAfter <= freshWithin {
		return nil, errors.New("subscription observer stale retirement threshold must be greater than the freshness window")
	}

	observerIDs, err := session.queryStrings(ctx, `
		SELECT candidate.observer_id
		FROM subscription_observers AS candidate
		WHERE candidate.tenant = $1
		  AND candidate.retired_at IS NULL
		  AND candidate.observer_id <> $2
		  AND candidate.observed_at < NOW() - ($3 * INTERVAL '1 second')
		  AND EXISTS (
			SELECT 1
			FROM subscription_observers AS peer
			WHERE peer.tenant = candidate.tenant
			  AND peer.retired_at IS NULL
			  AND peer.observer_id <> candidate.observer_id
			  AND peer.observed_at >= NOW() - ($4 * INTERVAL '1 second')
		  )
		ORDER BY candidate.observed_at
		FOR UPDATE OF candidate
	`, session.tenant, session.observerID, int(staleAfter.Seconds()), int(freshWithin.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("failed to select stale subscription observers: %w", err)
	}
	if len(observerIDs) == 0 {
		return nil, nil
	}

	retirements := make([]SubscriptionObserverRetirement, 0, len(observerIDs))
	for _, observerID := range observerIDs {
		updated, err := session.tx.ExecContext(ctx, `
			UPDATE subscription_observers
			SET retired_at = NOW()
			WHERE observer_id = $1
			  AND tenant = $2
			  AND retired_at IS NULL
		`, observerID, session.tenant)
		if err != nil {
			return nil, fmt.Errorf("failed to retire stale subscription observer: %w", err)
		}
		rowsAffected, err := updated.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("failed to count retired subscription observers: %w", err)
		}
		if rowsAffected == 0 {
			continue
		}

		retirement := SubscriptionObserverRetirement{
			ObserverID: observerID,
			Result:     SubscriptionCleanupRetained,
		}
		if err := session.cleanupIdentitiesForRetiredObserver(ctx, observerID, &retirement); err != nil {
			return nil, err
		}
		retirements = append(retirements, retirement)
	}
	return retirements, nil
}

func (session *postgresObservationSession) cleanupIdentitiesForRetiredObserver(
	ctx context.Context,
	observerID string,
	retirement *SubscriptionObserverRetirement,
) error {
	identityIDs, err := session.queryStrings(ctx, `
		SELECT identity.id
		FROM subscription_identities AS identity
		WHERE identity.tenant = $2
		  AND identity.deleted_at IS NULL
		  AND EXISTS (
			SELECT 1
			FROM subscription_observations AS observation
			WHERE observation.observer_id = $1
			  AND observation.subscription_id = identity.id
		  )
		ORDER BY identity.id
		FOR UPDATE OF identity
	`, observerID, session.tenant)
	if err != nil {
		return fmt.Errorf("failed to select identities for retired subscription observer: %w", err)
	}

	for _, identityID := range identityIDs {
		var present bool
		err := session.tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM subscription_observations AS observation
				JOIN subscription_observers AS observer
				  ON observer.observer_id = observation.observer_id
				WHERE observation.subscription_id = $1
				  AND observation.absent_at IS NULL
				  AND observer.retired_at IS NULL
				  AND observer.tenant = $2
			)
		`, identityID, session.tenant).Scan(&present)
		if err != nil {
			return fmt.Errorf("failed to check active observations after observer retirement: %w", err)
		}
		if present {
			continue
		}

		revocation, err := session.tx.ExecContext(ctx, `
			UPDATE api_keys
			SET deleted_at = NOW(),
			    status = CASE WHEN status = 'active' THEN 'revoked' ELSE status END
			WHERE subscription_id = $1 AND deleted_at IS NULL
		`, identityID)
		if err != nil {
			return fmt.Errorf("failed to revoke keys after observer retirement: %w", err)
		}
		rows, err := revocation.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to count revoked keys after observer retirement: %w", err)
		}
		if _, err := session.tx.ExecContext(ctx, `
			UPDATE subscription_identities
			SET deleted_at = NOW()
			WHERE id = $1 AND deleted_at IS NULL
		`, identityID); err != nil {
			return fmt.Errorf("failed to tombstone identity after observer retirement: %w", err)
		}
		retirement.Result = SubscriptionCleanupCleaned
		retirement.RevokedAPIKeys += int(rows)
	}
	return nil
}

func (session *postgresObservationSession) queryStrings(
	ctx context.Context,
	query string,
	args ...any,
) ([]string, error) {
	rows, err := session.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	values := make([]string, 0)
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func (session *postgresObservationSession) insertAbsentObservation(ctx context.Context, identityID, objectUID string) error {
	_, err := session.tx.ExecContext(ctx, `
		INSERT INTO subscription_observations (subscription_id, observer_id, object_uid, absent_at, observed_at)
		VALUES ($1, $2, $3, NOW(), NOW())
	`, identityID, session.observerID, objectUID)
	if err != nil {
		return fmt.Errorf("failed to insert absent subscription observation: %w", err)
	}
	return nil
}

func (session *postgresObservationSession) Commit() error {
	if err := session.tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit subscription observations: %w", err)
	}
	return nil
}

func (session *postgresObservationSession) Rollback() error {
	if err := session.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("failed to roll back subscription observations: %w", err)
	}
	return nil
}

// SubscriptionObservationMaxAge returns the configured freshness window.
