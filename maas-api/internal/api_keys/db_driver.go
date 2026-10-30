package api_keys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver
	"k8s.io/utils/env"

	"github.com/opendatahub-io/models-as-a-service/maas-api/db/schema"
	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/logger"
)

const (
	defaultMaxOpenConns        = 25
	defaultMaxIdleConns        = 5
	defaultConnMaxLifetimeSecs = 300
)

// concurrentMigration defines a migration that must run outside a transaction block.
// This is because golang-migrate wraps each migration in a transaction, which prevents statements
// like CREATE INDEX CONCURRENTLY. These run separately after the main migration runner.
type concurrentMigration struct {
	name string
	sql  string
}

// Append new concurrent migrations to this list.
var concurrentMigrations = []concurrentMigration{
	{
		name: "idx_api_keys_labels_gin",
		sql:  "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_api_keys_labels_gin ON api_keys USING GIN (labels jsonb_path_ops)",
	},
	// Future concurrent indexes go here.
}

// NewPostgresStoreFromURL creates a PostgreSQL store from a writable connection URL.
// It automatically applies database schema migrations on startup using golang-migrate.
// URL format: postgresql://user:password@host:port/database
// tenantName is used to filter all database queries to enforce tenant isolation.
func NewPostgresStoreFromURL(ctx context.Context, log *logger.Logger, databaseURL string, tenantName string) (*PostgresStore, error) {
	return NewPostgresStoreFromURLs(ctx, log, databaseURL, "", tenantName)
}

// NewPostgresStoreFromURLs creates a PostgreSQL store with separate writable and
// optional read-only connection pools. Schema migrations run only against the
// writable pool. An explicitly configured read pool must be reachable at startup.
//
// The two pools are intentionally not interchangeable. The writable pool owns schema
// state and every mutation. The read pool keeps the API-key validation hot path and
// administrative queries local to a cluster. Read routing is an explicit consistency
// contract: revocation visibility is bounded by asynchronous replication lag.
func NewPostgresStoreFromURLs(ctx context.Context, log *logger.Logger, writeURL, readURL string, tenantName string) (*PostgresStore, error) {
	writeURL = strings.TrimSpace(writeURL)
	readURL = strings.TrimSpace(readURL)

	if err := validateDatabaseURL(writeURL); err != nil {
		return nil, fmt.Errorf("invalid database URL: writable database %w", err)
	}
	if readURL != "" {
		if err := validateDatabaseURL(readURL); err != nil {
			return nil, fmt.Errorf("invalid database URL: read-only database %w", err)
		}
	}

	writeDB, err := sql.Open("pgx", writeURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open writable PostgreSQL connection: %w", err)
	}

	configureConnectionPool(writeDB)

	// Ping before migration so network, authentication, and TLS failures are reported as
	// connection failures rather than as confusing migration errors. Startup remains
	// fail-closed for the primary because no read replica can substitute for schema
	// ownership or writable state.
	if err := writeDB.PingContext(ctx); err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("failed to connect to writable PostgreSQL: %w", err)
	}

	// Apply ordinary and concurrent migrations before opening the read pool. PostgreSQL
	// replicas receive DDL through replication; executing it directly against a read-only
	// endpoint would fail and could also hide a topology where the replica is not actually
	// following this primary.
	if err := runMigrations(writeDB, log); err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("failed to apply schema migrations: %w", err)
	}
	if err := runConcurrentMigrations(ctx, writeDB, log); err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("failed to apply concurrent migrations: %w", err)
	}

	readDB := writeDB
	if readURL != "" {
		readDB, err = sql.Open("pgx", readURL)
		if err != nil {
			writeDB.Close()
			return nil, fmt.Errorf("failed to open read-only PostgreSQL connection: %w", err)
		}
		configureConnectionPool(readDB)
		// A configured read endpoint is also fail-closed at startup. Falling back silently
		// here would make a GitOps typo or missing Service look like a successful rollout
		// while every local read, including API-key validation, crosses clusters to
		// the primary.
		if err := readDB.PingContext(ctx); err != nil {
			readDB.Close()
			writeDB.Close()
			return nil, fmt.Errorf("failed to connect to read-only PostgreSQL: %w", err)
		}
		log.Info("Connected to PostgreSQL read replica", "tenant", tenantName)
	}

	log.Info("Connected to PostgreSQL database (schema applied)", "tenant", tenantName, "read_replica_configured", readURL != "")
	store := &PostgresStore{
		writeDB:    writeDB,
		readDB:     readDB,
		logger:     log,
		tenantName: tenantName,
		metrics:    newStoreMetrics(),
	}
	store.readHealthy.Store(true)
	return store, nil
}

func validateDatabaseURL(databaseURL string) error {
	if !strings.HasPrefix(databaseURL, "postgresql://") && !strings.HasPrefix(databaseURL, "postgres://") {
		return errors.New("must start with postgresql:// or postgres://")
	}
	return nil
}

// runMigrations applies database schema migrations using golang-migrate.
func runMigrations(db *sql.DB, log *logger.Logger) error {
	// Create migration source from embedded schema files
	source, err := iofs.New(schema.FS, ".")
	if err != nil {
		return fmt.Errorf("failed to create schema migration source: %w", err)
	}

	// Create database driver for schema migrations
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("failed to create schema migration driver: %w", err)
	}

	// Create schema migrator
	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		return fmt.Errorf("failed to create schema migrator: %w", err)
	}

	// Run schema migrations
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("schema migration failed: %w", err)
	}

	version, dirty, _ := m.Version()
	if dirty {
		log.Warn("Database schema is in dirty state", "version", version)
	} else {
		log.Info("Database schema applied", "version", version)
	}

	return nil
}

// configureConnectionPool sets optimal connection pool settings.
func configureConnectionPool(db *sql.DB) {
	maxOpenConns, _ := env.GetInt("DB_MAX_OPEN_CONNS", defaultMaxOpenConns)
	maxIdleConns, _ := env.GetInt("DB_MAX_IDLE_CONNS", defaultMaxIdleConns)
	connMaxLifetimeSecs, _ := env.GetInt("DB_CONN_MAX_LIFETIME_SECONDS", defaultConnMaxLifetimeSecs)

	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(time.Duration(connMaxLifetimeSecs) * time.Second)
}

// Return the names of the concurrent migrations. (A convenience getter that supports better schema-migration integration testing.)
func ConcurrentMigrationNames() []string {
	names := make([]string, len(concurrentMigrations))
	for i, m := range concurrentMigrations {
		names[i] = m.name
	}
	return names
}

// Apply schema migrations that cannot be run inside a transaction block and must be run concurrently.
// If they can run inside a transaction block then the correct place is in the runMigrations function.
func runConcurrentMigrations(ctx context.Context, db *sql.DB, log *logger.Logger) error {
	for _, m := range concurrentMigrations {
		log.Info("Applying concurrent migration", "name", m.name)
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("concurrent migration %q failed: %w", m.name, err)
		}
	}
	return nil
}
