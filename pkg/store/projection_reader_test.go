package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gta/pkg/event"
)

func TestSQLiteStore_QueryMetrics(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	metrics := []event.Metric{
		{Name: "count", Window: time.Now(), Group: map[string]string{"proto": "tcp"}, Value: 10},
		{Name: "count", Window: time.Now().Add(time.Second), Group: map[string]string{"proto": "udp"}, Value: 5},
		{Name: "bytes", Window: time.Now(), Group: map[string]string{"proto": "tcp"}, Value: 1024},
	}
	if err := s.WriteMetrics(ctx, metrics); err != nil {
		t.Fatal(err)
	}

	// 查全部
	rows, err := s.QueryMetrics(ctx, MetricQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("QueryMetrics all: got %d, want 3", len(rows))
	}

	// 按 name 过滤
	rows, err = s.QueryMetrics(ctx, MetricQuery{Name: "count"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("QueryMetrics count: got %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Group["proto"] != "tcp" && r.Group["proto"] != "udp" {
			t.Errorf("unexpected group: %v", r.Group)
		}
	}
}

