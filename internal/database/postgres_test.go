package database

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindMigrationDirPrefersRepoBackendMigrationsFromRoot(t *testing.T) {
	root := t.TempDir()
	migrationDir := filepath.Join(root, "backend", "migrations")

	if err := os.MkdirAll(migrationDir, 0o755); err != nil {
		t.Fatalf("failed to create migrations dir: %v", err)
	}

	dir, checked, err := findMigrationDir(root)
	if err != nil {
		t.Fatalf("findMigrationDir returned error: %v", err)
	}
	if dir != migrationDir {
		t.Fatalf("findMigrationDir returned %q, want %q", dir, migrationDir)
	}
	if len(checked) < 2 {
		t.Fatalf("findMigrationDir checked %d paths, want at least 2", len(checked))
	}
}

func TestFindMigrationDirUsesLocalMigrationsWhenRunningInsideBackend(t *testing.T) {
	root := t.TempDir()
	backendDir := filepath.Join(root, "backend")
	migrationDir := filepath.Join(backendDir, "migrations")

	if err := os.MkdirAll(migrationDir, 0o755); err != nil {
		t.Fatalf("failed to create migrations dir: %v", err)
	}

	dir, _, err := findMigrationDir(backendDir)
	if err != nil {
		t.Fatalf("findMigrationDir returned error: %v", err)
	}
	if dir != migrationDir {
		t.Fatalf("findMigrationDir returned %q, want %q", dir, migrationDir)
	}
}

func TestFindMigrationDirReturnsErrorWhenMissing(t *testing.T) {
	root := t.TempDir()

	if _, _, err := findMigrationDir(root); err == nil {
		t.Fatal("findMigrationDir returned nil error for missing migrations directory")
	}
}
