 #!/usr/bin/env bash
  set -euo pipefail

  CONTAINER_NAME="maas-test"
  DB_NAME="maas_test"
  PG_USER="postgres"
  PG_PASS="postgres"
  PG_PORT="5432"
  PG_IMAGE="postgres:15"
  MAX_RETRIES=30

  cleanup() {
      echo "Cleaning up..."
      podman stop "$CONTAINER_NAME" 2>/dev/null || true
      podman rm "$CONTAINER_NAME" 2>/dev/null || true
  }
  trap cleanup EXIT

  # Remove any leftover container from a prior run
  podman rm -f "$CONTAINER_NAME" 2>/dev/null || true

  echo "Starting PostgreSQL container..."
  podman run --name "$CONTAINER_NAME" \
      -e POSTGRES_PASSWORD="$PG_PASS" \
      -p "${PG_PORT}:5432" \
      -d "$PG_IMAGE"

  echo "Waiting for PostgreSQL readiness..."
  for i in $(seq 1 "$MAX_RETRIES"); do
      if podman exec "$CONTAINER_NAME" pg_isready -U "$PG_USER" -q 2>/dev/null; then
          echo "PostgreSQL ready after ${i}s"
          break
      fi
      if [ "$i" -eq "$MAX_RETRIES" ]; then
          echo "PostgreSQL failed to start after ${MAX_RETRIES}s"
          exit 1
      fi
      sleep 1
  done

  echo "Creating test database..."
  podman exec "$CONTAINER_NAME" psql -U "$PG_USER" -c "CREATE DATABASE ${DB_NAME};"

  echo "Running integration tests..."
  export TEST_DATABASE_URL="postgres://${PG_USER}:${PG_PASS}@localhost:${PG_PORT}/${DB_NAME}?sslmode=disable"
  cd "$(git rev-parse --show-toplevel)/maas-api"
  go test -tags=integration -v -run "TestPostgres" ./internal/api_keys/
