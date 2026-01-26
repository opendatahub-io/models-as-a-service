package api_keys

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver
	_ "github.com/mattn/go-sqlite3"    // SQLite driver
	"k8s.io/utils/env"

	"github.com/go-logr/logr"
)

type DBType string

const (
	DBTypeSQLite   DBType = "sqlite"
	DBTypePostgres DBType = "postgres"

	driverSQLite   = "sqlite3"
	driverPostgres = "pgx"

	sqliteMemory = ":memory:"
)

// Currently supports PostgreSQL only.
func NewExternalStore(ctx context.Context, log logr.Logger, databaseURL string) (*SQLStore, error) {
	databaseURL = strings.TrimSpace(databaseURL)

	if !strings.HasPrefix(databaseURL, "postgresql://") && !strings.HasPrefix(databaseURL, "postgres://") {
		return nil, fmt.Errorf(
			"unsupported external database URL: %q. Currently supported: postgresql://",
			databaseURL)
	}

	db, err := sql.Open(driverPostgres, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open PostgreSQL database: %w", err)
	}

	configureConnectionPool(db, DBTypePostgres)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to connect to PostgreSQL database: %w", err)
	}

	s := &SQLStore{db: db, dbType: DBTypePostgres, logger: log}
	if err := s.initSchema(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	log.Info("Connected to external PostgreSQL database")
	return s, nil
}

// placeholder returns the appropriate SQL placeholder for the database type.
// SQLite uses ?, PostgreSQL uses $1, $2, etc.
func placeholder(dbType DBType, index int) string {
	if dbType == DBTypeSQLite {
		return "?"
	}
	return fmt.Sprintf("$%d", index)
}

const (
	defaultMaxOpenConns        = 25
	defaultMaxIdleConns        = 5
	defaultConnMaxLifetimeSecs = 300
)

// configureConnectionPool sets optimal connection pool settings based on database type.
func configureConnectionPool(db *sql.DB, dbType DBType) {
	if dbType == DBTypePostgres {
		maxOpenConns, _ := env.GetInt("DB_MAX_OPEN_CONNS", defaultMaxOpenConns)
		maxIdleConns, _ := env.GetInt("DB_MAX_IDLE_CONNS", defaultMaxIdleConns)
		connMaxLifetimeSecs, _ := env.GetInt("DB_CONN_MAX_LIFETIME_SECONDS", defaultConnMaxLifetimeSecs)

		db.SetMaxOpenConns(maxOpenConns)
		db.SetMaxIdleConns(maxIdleConns)
		db.SetConnMaxLifetime(time.Duration(connMaxLifetimeSecs) * time.Second)
	} else {
		// SQLite: single connection to avoid database locking issues
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
	}
}
