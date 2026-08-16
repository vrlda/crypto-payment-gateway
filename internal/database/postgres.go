package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

var DB *pgxpool.Pool

func InitDB() error {
	dbUrl := os.Getenv("DATABASE_URL")
	if dbUrl == "" {
		return fmt.Errorf("DATABASE_URL environment variable is not set")
	}

	config, err := pgxpool.ParseConfig(dbUrl)
	if err != nil {
		return fmt.Errorf("unable to parse DB config: %w", err)
	}

	// Pool settings for performance
	config.MaxConns = 50
	config.MinConns = 10

	DB, err = pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return fmt.Errorf("unable to create connection pool: %w", err)
	}

	// Verify connection
	if err := DB.Ping(context.Background()); err != nil {
		return fmt.Errorf("unable to ping database: %w", err)
	}

	fmt.Println("Connected to Database successfully")

	// Run Migrations
	if err := runMigrations(dbUrl); err != nil {
		return err
	}

	return nil
}

func runMigrations(dbUrl string) error {
	cwd, _ := os.Getwd()

	migrationDir, checkedPaths, err := findMigrationDir(cwd)
	if err != nil {
		return fmt.Errorf("failed to locate migrations directory from %s (checked: %s): %w", cwd, strings.Join(checkedPaths, ", "), err)
	}

	migrationPath := "file://" + filepath.ToSlash(migrationDir)
	fmt.Printf("Current Working Directory: %s\n", cwd)
	fmt.Printf("Using migration path: %s\n", migrationPath)

	// List files in the directory for debugging
	files, _ := os.ReadDir(migrationDir)
	fmt.Printf("Found %d files in %s:\n", len(files), migrationDir)
	for _, f := range files {
		fmt.Println(" - " + f.Name())
	}

	m, err := migrate.New(migrationPath, dbUrl)
	if err != nil {
		return fmt.Errorf("failed to init migrate: %w", err)
	}

	// Try to run migrations
	if err := m.Up(); err != nil {
		if err == migrate.ErrNoChange {
			// No changes needed
			return nil
		}

		if strings.HasPrefix(err.Error(), "Dirty database") {
			return fmt.Errorf("database migration state is dirty; manual intervention is required before startup: %w", err)
		}

		return fmt.Errorf("failed to run migrate up: %w", err)
	}

	fmt.Println("Migrations executed successfully")
	return nil
}

func findMigrationDir(cwd string) (string, []string, error) {
	candidates := []string{
		"migrations",
		"backend/migrations",
		"../migrations",
		"../backend/migrations",
		"../../migrations",
		"../../backend/migrations",
	}

	checked := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		fullPath := filepath.Clean(filepath.Join(cwd, candidate))
		checked = append(checked, fullPath)

		info, err := os.Stat(fullPath)
		if err == nil && info.IsDir() {
			return fullPath, checked, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", checked, err
		}
	}

	return "", checked, fmt.Errorf("migrations directory not found")
}
