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

// NewPostgresStoreFromURL creates a PostgreSQL store from a connection URL.
// It automatically applies database schema migrations on startup using golang-migrate.
// URL format: postgresql://user:password@host:port/database
// tenantName is used to filter all database queries to enforce tenant isolation.
func NewPostgresStoreFromURL(ctx context.Context, log *logger.Logger, databaseURL string, tenantName string) (*PostgresStore, error) {
	databaseURL = strings.TrimSpace(databaseURL)

	if !strings.HasPrefix(databaseURL, "postgresql://") && !strings.HasPrefix(databaseURL, "postgres://") {
		return nil, errors.New(
			"invalid database URL: must start with postgresql:// or postgres://")
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open PostgreSQL connection: %w", err)
	}

	configureConnectionPool(db)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}

	// Apply schema migrations
	if err := runMigrations(db, log); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to apply schema migrations: %w", err)
	}
	if err := runConcurrentMigrations(ctx, db, log); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to apply concurrent migrations: %w", err)
	}

	log.Info("Connected to PostgreSQL database (schema applied)", "tenant", tenantName)
	return &PostgresStore{db: db, logger: log, tenantName: tenantName}, nil
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