package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gametrace/pkg/event"
)

func TestSQLiteStoreMetrics(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.WriteMetrics(ctx, []event.Metric{
		{Name: "m1", Window: time.Now(), Group: map[string]string{"g": "a"}, Value: 1},
	}); err != nil {
		t.Fatal(err)
	}
}
