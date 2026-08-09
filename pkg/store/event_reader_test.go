package store

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"gta/pkg/event"
)

func TestSQLiteStore_QueryEventsV2(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	now := time.Now()

	// 创建 Event 测试数据
	payload1 := event.Value{
		Kind: event.Object,
		Object: map[string]event.Value{
			"src":       {Kind: event.String, Str: "127.0.0.1:5000"},
			"dst":       {Kind: event.String, Str: "127.0.0.1:8080"},
			"flow_id":   {Kind: event.Uint, Uint: 100},
			"direction": {Kind: event.String, Str: "client_to_server"},
			"msg_name":  {Kind: event.String, Str: "Login"},
			"a":         {Kind: event.Int, Int: 1},
		},
	}
	payload2 := event.Value{
		Kind: event.Object,
		Object: map[string]event.Value{
			"src":       {Kind: event.String, Str: "127.0.0.1:8080"},
			"dst":       {Kind: event.String, Str: "127.0.0.1:5000"},
			"flow_id":   {Kind: event.Uint, Uint: 100},
			"direction": {Kind: event.String, Str: "server_to_client"},
			"msg_name":  {Kind: event.String, Str: "LoginResp"},
			"b":         {Kind: event.Int, Int: 2},
		},
	}
	payload3 := event.Value{
		Kind: event.Object,
		Object: map[string]event.Value{
			"c": {Kind: event.Int, Int: 3},
		},
	}

	events := []*event.Event{
		{
			Identity: event.Identity{
				ID:        "e1",
				SessionID: "s1",
				Type:      "tcp",
				SchemaID:  "tcp.v1",
				Source:    "test",
				Timestamp: now,
			},
			Relation: event.Relation{},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value:    payload1,
			},
		},
		{
			Identity: event.Identity{
				ID:        "e2",
				SessionID: "s1",
				Type:      "tcp",
				SchemaID:  "tcp.v1",
				Source:    "test",
				Timestamp: now.Add(time.Millisecond),
			},
			Relation: event.Relation{},
			Payload: event.Payload{
				SchemaID: "tcp.v1",
				Value:    payload2,
			},
		},
		{
			Identity: event.Identity{
				ID:        "e3",
				SessionID: "s2",
				Type:      "udp",
				SchemaID:  "udp.v1",
				Source:    "test",
				Timestamp: now.Add(2 * time.Millisecond),
			},
			Relation: event.Relation{},
			Payload: event.Payload{
				SchemaID: "udp.v1",
				Value:    payload3,
			},
		},
	}

	if err := s.AppendEvents(ctx, events); err != nil {
		t.Fatal(err)
	}

	// 查全部 s1 事件
	rows, err := s.QueryEvents(ctx, "s1", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("QueryEvents s1: got %d rows, want 2", len(rows))
	}
	if string(rows[0].Identity.ID) != "e1" {
		t.Errorf("rows[0].Identity.ID = %q, want e1", rows[0].Identity.ID)
	}

	// 验证 payload 中的字段
	obj1, ok := rows[0].Payload.Value.AsObject()
	if !ok {
		t.Fatal("rows[0].Payload.Value is not an object")
	}
	if src, exists := obj1["src"]; !exists || src.Str != "127.0.0.1:5000" {
		t.Errorf("rows[0] src = %v, want 127.0.0.1:5000", src)
	}
	if dir, exists := obj1["direction"]; !exists || dir.Str != "client_to_server" {
		t.Errorf("rows[0] direction = %v, want client_to_server", dir)
	}
	if msgName, exists := obj1["msg_name"]; !exists || msgName.Str != "Login" {
		t.Errorf("rows[0] msg_name = %v, want Login", msgName)
	}

	// 查 e3（s2 session）
	rows, err = s.QueryEvents(ctx, "s2", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || string(rows[0].Identity.ID) != "e3" {
		t.Fatalf("QueryEvents s2: got %+v, want e3", rows)
	}

	// 测试 Limit
	rows, err = s.QueryEvents(ctx, "s1", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("QueryEvents limit: got %d, want 1", len(rows))
	}

	// 测试 Offset
	rows, err = s.QueryEvents(ctx, "s1", 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("QueryEvents offset: got %d, want 1", len(rows))
	}
	if string(rows[0].Identity.ID) != "e2" {
		t.Errorf("rows[0].Identity.ID = %q, want e2", rows[0].Identity.ID)
	}
}

func TestSQLiteStore_GetSchema(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	info, err := s.GetSchema(ctx, "ignored")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tbl := range info.Tables {
		names[tbl.Name] = true
		if len(tbl.Columns) == 0 {
			t.Errorf("table %q has no columns", tbl.Name)
		}
	}
	for _, want := range []string{"raw_packets", "events", "aggregated_metrics", "state_changes", "event_index"} {
		if !names[want] {
			t.Errorf("schema missing table %q", want)
		}
	}
}

func TestSQLiteStore_RawQuery(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// 用 RawQuery 查 sqlite_master
	rows, err := s.RawQuery(ctx, "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 4 {
		t.Errorf("RawQuery: got %d tables, want >=4", len(rows))
	}
}

func TestSQLiteStore_QueryRawPackets(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// 写入 raw packets
	pkts := []event.Packet{
		{Timestamp: time.Now(), Src: netip.AddrPort{}, Dst: netip.AddrPort{}, Protocol: "tcp", Raw: []byte("hello")},
		{Timestamp: time.Now().Add(time.Millisecond), Src: netip.AddrPort{}, Dst: netip.AddrPort{}, Protocol: "udp", Raw: []byte("world")},
	}
	if err := s.AppendRawPackets(ctx, pkts); err != nil {
		t.Fatal(err)
	}

	// 查全部
	rows, err := s.QueryRawPackets(ctx, RawPacketQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("QueryRawPackets all: got %d, want 2", len(rows))
	}

	// Limit
	rows, err = s.QueryRawPackets(ctx, RawPacketQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("QueryRawPackets limit: got %d, want 1", len(rows))
	}
}
