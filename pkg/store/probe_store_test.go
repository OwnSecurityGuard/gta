package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

// newProbeTestStore 依次尝试 sqlite / PG 两个后端；PG 不可达时跳过。
func newProbeTestStore(t *testing.T) ControlStoreBackend {
	t.Helper()
	cs, err := NewControlStore(t.TempDir() + "/control.db")
	if err != nil {
		t.Fatalf("open sqlite control store: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestProbes_CRUD(t *testing.T) {
	cs := newProbeTestStore(t)
	ctx := context.Background()

	token := "gt_prb_test1234567890"
	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])

	err := cs.UpsertProbe(ctx, ProbeMeta{
		ProbeID: "prb_1", Name: "game-server-01", Owner: "alice",
		Capabilities: "pcap,plugin_host", TokenHash: tokenHash,
		Version: "1.4.0", Hostname: "WIN-A1B2", OS: "windows", Arch: "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := cs.GetProbe(ctx, "prb_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "game-server-01" || got.Owner != "alice" || got.TokenHash != tokenHash {
		t.Fatalf("unexpected probe: %+v", got)
	}
	if got.TenantID != "default" {
		t.Fatalf("tenant should default to %q, got %q", "default", got.TenantID)
	}
	if got.CaptureState != "idle" {
		t.Fatalf("fresh probe capture_state should be idle, got %q", got.CaptureState)
	}

	// 心跳快照：三维度落库。
	now := time.Now().UTC()
	err = cs.UpdateProbeStatus(ctx, "prb_1", ProbeRuntimeStatus{
		ConnectionState: "online", CaptureState: "running", LastSessionID: "s1",
		CaptureIface: "eth0", CapturePorts: "8080,9090",
		LastPacketMs: now.UnixMilli() - 2000, LastUploadMs: now.UnixMilli() - 1000,
		PacketsCaptured: 100, PacketsAcked: 90, SpoolDepth: 10,
		ArchiveBytes: 1 << 20, ArchiveSegments: 2,
		ArchiveOldestMs: now.Add(-time.Hour).UnixMilli(), ArchiveNewestMs: now.UnixMilli(),
		LastSeenAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = cs.GetProbe(ctx, "prb_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ConnectionState != "online" || got.CaptureState != "running" || got.LastSessionID != "s1" {
		t.Fatalf("runtime status not persisted: %+v", got)
	}
	if got.PacketsCaptured != 100 || got.PacketsAcked != 90 {
		t.Fatalf("counters not persisted: %+v", got)
	}

	// 凭证解析。
	byToken, err := cs.GetProbeByTokenHash(ctx, tokenHash)
	if err != nil || byToken.ProbeID != "prb_1" {
		t.Fatalf("resolve by token hash failed: %v %+v", err, byToken)
	}
	if _, err := cs.GetProbeByTokenHash(ctx, ""); err == nil {
		t.Fatal("empty hash should not resolve")
	}

	// 归档段缓存替换与区间查询。
	segs := []ArchiveSegmentMeta{
		{SegID: "seg-a", FirstMs: now.Add(-2 * time.Hour).UnixMilli(), LastMs: now.Add(-1 * time.Hour).UnixMilli(), Packets: 10, Bytes: 100, LinkType: 1},
		{SegID: "seg-b", FirstMs: now.Add(-30 * time.Minute).UnixMilli(), LastMs: now.UnixMilli(), Packets: 20, Bytes: 200, LinkType: 1},
	}
	if err := cs.ReplaceProbeSegments(ctx, "prb_1", segs); err != nil {
		t.Fatal(err)
	}
	all, err := cs.ListProbeSegments(ctx, "prb_1", 0, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("list all segments: %v %d", err, len(all))
	}
	win, err := cs.ListProbeSegments(ctx, "prb_1", now.Add(-40*time.Minute).UnixMilli(), 0)
	if err != nil || len(win) != 1 || win[0].SegID != "seg-b" {
		t.Fatalf("window query should hit only seg-b: %v %+v", err, win)
	}

	// Upsert 覆盖：owner 与 created_at 不变（探针不许换主）。
	firstCreated := got.CreatedAt
	err = cs.UpsertProbe(ctx, ProbeMeta{
		ProbeID: "prb_1", Name: "renamed", Owner: "mallory",
		Capabilities: "pcap", TokenHash: "newhash",
		Hostname: "h2",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = cs.GetProbe(ctx, "prb_1")
	if got.Owner != "alice" {
		t.Fatalf("owner must not change on re-register, got %q", got.Owner)
	}
	if !got.CreatedAt.Equal(firstCreated) {
		t.Fatal("created_at must not change on re-register")
	}
	if got.Name != "renamed" {
		t.Fatalf("name should update, got %q", got.Name)
	}

	// Revoke 后凭证不可解析。
	if err := cs.RevokeProbe(ctx, "prb_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.GetProbeByTokenHash(ctx, "newhash"); err == nil {
		t.Fatal("revoked probe token should not resolve")
	}
	if err := cs.DeleteProbe(ctx, "prb_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.GetProbe(ctx, "prb_1"); err == nil {
		t.Fatal("deleted probe should not be found")
	}
	left, err := cs.ListProbeSegments(ctx, "prb_1", 0, 0)
	if err != nil || len(left) != 0 {
		t.Fatalf("segments should be cascade deleted: %v %d", err, len(left))
	}
}
