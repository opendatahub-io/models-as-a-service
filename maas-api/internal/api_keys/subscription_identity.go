package api_keys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var ErrSubscriptionIdentityPending = errors.New("subscription identity rotation is pending")

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
	ResolveSubscriptionIdentity(ctx context.Context, request ResolveSubscriptionIdentityRequest) (string, error)
	MarkAbsent(ctx context.Context, subscriptionID string) error
	RecordAbsenceAndCleanup(ctx context.Context, request SubscriptionAbsenceRequest, settle, maxObservationAge time.Duration) (SubscriptionCleanupResult, int, error)
	Commit() error
	Rollback() error
}

type postgresObservationSession struct {
	tx         *sql.Tx
	store      *PostgresStore
	observerID string
	tenant     string
}

var _ SubscriptionIdentityStore = (*PostgresStore)(nil)

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
	if _, err := session.tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", session.tenant+":"+request.Namespace+":"+request.Name); err != nil {
		return "", fmt.Errorf("failed to acquire subscription identity lock: %w", err)
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
	if priorID != "" {
		if err := session.rotateIdentity(ctx, identityID); err != nil {
			return "", err
		}
		return "", ErrSubscriptionIdentityPending
	}

	priorObjectUID, priorID, err := session.priorObservationForName(ctx, request)
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

func (session *postgresObservationSession) RecordAbsenceAndCleanup(
	ctx context.Context,
	request SubscriptionAbsenceRequest,
	settle, maxObservationAge time.Duration,
) (SubscriptionCleanupResult, int, error) {
	if request.Namespace == "" || request.Name == "" || request.ObjectUID == "" {
		return "", 0, errors.New("subscription namespace, name, and object UID are required")
	}
	if request.SubscriptionIdentityID != "" {
		if _, err := uuid.Parse(request.SubscriptionIdentityID); err != nil {
			return "", 0, fmt.Errorf("%w: invalid UUID", ErrSubscriptionIdentityMismatch)
		}
	}
	if settle <= 0 || maxObservationAge <= 0 {
		return "", 0, errors.New("settle interval and maximum observation age must be positive")
	}

	if _, err := session.tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", session.tenant+":"+request.Namespace+":"+request.Name); err != nil {
		return "", 0, fmt.Errorf("failed to acquire subscription identity lock: %w", err)
	}

	identityID := request.SubscriptionIdentityID
	if identityID == "" {
		if err := session.tx.QueryRowContext(ctx, `
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
			`, session.observerID, request.ObjectUID, session.tenant, request.Namespace, request.Name).Scan(&identityID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				if err := session.tx.QueryRowContext(ctx, `
					SELECT id
					FROM subscription_identities
					WHERE tenant = $1 AND namespace = $2 AND name = $3 AND deleted_at IS NULL
					FOR UPDATE
				`, session.tenant, request.Namespace, request.Name).Scan(&identityID); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return SubscriptionCleanupCleaned, 0, nil
					}
					return "", 0, fmt.Errorf("failed to lock subscription identity by name for cleanup: %w", err)
				}
				return SubscriptionCleanupBlockedUnverified, 0, nil
			}
			if err != nil {
				return "", 0, fmt.Errorf("failed to recover subscription identity for absence: %w", err)
			}
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
		return SubscriptionCleanupNotFound, 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("failed to lock subscription identity for cleanup: %w", err)
	}
	if deletedAt.Valid {
		return SubscriptionCleanupCleaned, 0, nil
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
			return "", 0, err
		}
	} else if err != nil {
		return "", 0, fmt.Errorf("failed to read subscription observation for cleanup: %w", err)
	} else if !absentAt.Valid && observedObjectUID != request.ObjectUID {
		return "", 0, ErrSubscriptionIdentityMismatch
	}

	if err := session.MarkAbsent(ctx, identityID); err != nil {
		return "", 0, err
	}
	if request.RetainAPIKeys {
		return SubscriptionCleanupRetainedPolicy, 0, nil
	}

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
	err = session.tx.QueryRowContext(ctx, `
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
		SELECT MIN(observation.absent_at + ($2 * INTERVAL '1 second'))
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
