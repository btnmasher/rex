package main

import (
	"context"
	"testing"
)

func TestConfigureMemoryJobStoreDoesNotRequirePostgres(t *testing.T) {
	store, closeStore, err := configureJobStore(context.Background(), "memory", "", nil)
	if err != nil {
		t.Fatalf("configure memory job store: %v", err)
	}
	if store == nil {
		t.Fatal("configure memory job store returned nil store")
	}
	closeStore()
}

func TestConfigureSQLiteJobStoreDoesNotRequirePostgres(t *testing.T) {
	store, closeStore, err := configureJobStore(context.Background(), "sqlite", t.TempDir()+"/job.sqlite", nil)
	if err != nil {
		t.Fatalf("configure SQLite job store: %v", err)
	}
	if store == nil {
		t.Fatal("configure SQLite job store returned nil store")
	}
	closeStore()
}
