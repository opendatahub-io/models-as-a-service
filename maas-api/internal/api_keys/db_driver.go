package api_keys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
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

// NewPostgresStoreFromURL creates a PostgreSQL store from a connection URL.
// It automatically applies database schema migrations on startup using golang-migrate.
// URL format: postgresql://user:password@host:port/database
// tenantName is used to filter all database queries to enforce tenant isolation.
func NewPostgresStoreFromURL(ctx context.Context, log *logger.Logger, databaseURL string, tenantName string) (*PostgresStore, error) {
	databaseURL = strings.TrimSpace(databaseURL)

	if !strings.HasPrefix(databaseURL, "postgresql://") && !strings.HasPrefix(databaseURL, "postgres://") {
		return nil, fmt.Errorf(
			"invalid database URL scheme. Expected format: postgresql://user:password@host:port/database")
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open PostgreSQL connection: %s", redactDatabaseURL(err.Error(), databaseURL))
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

// redactDatabaseURL removes userinfo (user:password) from any occurrence of
// the database URL in a message to prevent credential leakage into logs.
func redactDatabaseURL(message, databaseURL string) string {
	u, err := url.Parse(databaseURL)
	if err != nil || u.User == nil {
		return message
	}
	redacted := *u
	redacted.User = url.User("[REDACTED]")
	return strings.ReplaceAll(message, databaseURL, redacted.Redacted())
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
