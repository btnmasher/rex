package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunPostgresMigrationRequiresDatabaseURL(t *testing.T) {
	t.Setenv("JOB_STORE", "sqlite")
	t.Setenv("DATABASE_URL", "")

	err := runMigrations(context.Background(), []string{"-store", "postgres"})
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Fatalf("run() error = %v, want DATABASE_URL validation", err)
	}
}
