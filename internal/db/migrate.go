package db

import (
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	// pgx driver for the database connection in migrate
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
)

// migrationsFS embeds the SQL migration files into the binary at compile time.
// Path is relative to THIS file's package directory.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies all pending up-migrations to bring the DB to latest schema.
//
// TODO (you): implement this function.
//
// Requirements:
//   1. Create an iofs.New(migrationsFS, "migrations") source — exposes the embedded files
//      as a migrate.Source.
//   2. Call migrate.NewWithSourceInstance("iofs", src, dbURL)
//      - dbURL must use scheme "pgx5://" for the pgx5 driver
//      - if dbURL starts with "postgres://", rewrite the prefix to "pgx5://"
//   3. Defer m.Close() to release the source + db handles.
//   4. Call m.Up().
//      - if errors.Is(err, migrate.ErrNoChange) → no migrations needed, return nil.
//      - other err → wrap with fmt.Errorf("migrate.up: %w", err).
//   5. Return nil on success.
//
// Important: call BEFORE db.Open() in main(). Migrations run on their own
// connection — concurrent with an open pool can cause lock contention.
//
// IMPORTANT: dbURL must use scheme "pgx5://" for the pgx/v5 migrate driver.
// If your config DB_URL starts with "postgres://", convert it: strings.Replace
// "postgres://" with "pgx5://" before passing to migrate.NewWithSourceInstance.
func Migrate(dbURL string) error {
	// TODO: implement
	//
	// Step 1: source from embedded FS
	//   src, err := iofs.New(migrationsFS, "migrations")
	//   if err != nil { return fmt.Errorf("migrate.iofs: %w", err) }
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("migrate.iofs: %w", err)
	}
	defer src.Close()
	
	//
	// Step 2: rewrite URL scheme for migrate driver
	//   migURL := strings.Replace(dbURL, "postgres://", "pgx5://", 1)
	migURL := strings.Replace(dbURL, "postgres://", "pgx5://", 1)
	//
	// Step 3: build migrator
	//   m, err := migrate.NewWithSourceInstance("iofs", src, migURL)
	//   if err != nil { return fmt.Errorf("migrate.new: %w", err) }
	//   defer m.Close()
	m, err := migrate.NewWithSourceInstance("iofs", src, migURL)
	if err != nil {
		return fmt.Errorf("migrate.new: %w", err)
	}
	defer m.Close()
	//
	// Step 4: apply
	//   err = m.Up()
	//   if err != nil && !errors.Is(err, migrate.ErrNoChange) {
	//       return fmt.Errorf("migrate.up: %w", err)
	//   }
	//   return nil
	err = m.Up()
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate.up: %w", err)
	}
	return nil
}
