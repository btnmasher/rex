package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunRequiresPostgresJobStore(t *testing.T) {
	t.Setenv("JOB_STORE_SQLITE_PATH", t.TempDir()+"/job.sqlite")
	t.Setenv("JOB_STORE", "sqlite")
	t.Setenv("DATABASE_URL", "")

	err := runMigrations(context.Background(), []string{"-store", "postgres"})
	if err == nil || !strings.Contains(err.Error(), "JOB_STORE must be postgres") {
		t.Fatalf("run() error = %v, want PostgreSQL job-store guard", err)
	}
}
