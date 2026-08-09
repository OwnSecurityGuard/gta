package pcapfile

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gta/pkg/capture"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"
)

func writeTestPcap(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := pcapgo.NewWriter(f)
	_ = w.WriteFileHeader(65536, layers.LinkTypeEthernet)

	eth := layers.Ethernet{SrcMAC: net.HardwareAddr{1, 2, 3, 4, 5, 6}, DstMAC: net.HardwareAddr{6, 5, 4, 3, 2, 1}, EthernetType: layers.EthernetTypeIPv4}
	ip4 := layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolTCP, SrcIP: net.IP{10, 0, 0, 1}, DstIP: net.IP{10, 0, 0, 2}}
	tcp := layers.TCP{SrcPort: 12345, DstPort: 80}
	_ = tcp.SetNetworkLayerForChecksum(&ip4)
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{ComputeChecksums: true, FixLengths: true}
	_ = gopacket.SerializeLayers(buf, opts, &eth, &ip4, &tcp, gopacket.Payload([]byte("hello")))
	_ = w.WritePacket(gopacket.CaptureInfo{Timestamp: time.Unix(1, 0), Length: len(buf.Bytes()), CaptureLength: len(buf.Bytes())}, buf.Bytes())
}

func TestOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.pcap")
	writeTestPcap(t, path)

	src, err := capture.Open(context.Background(), "pcap-file", PcapFileConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	pkt := <-src.Packets()
	if pkt.Protocol != "tcp" {
		t.Fatalf("expected tcp, got %s", pkt.Protocol)
	}
	if pkt.Metadata == nil {
		t.Fatal("metadata should not be nil")
	}
	if src.State() != capture.StateRunning {
		t.Fatalf("expected state running, got %s", src.State())
	}
}

func TestCloseIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.pcap")
	writeTestPcap(t, path)

	src, err := capture.New("pcap-file", PcapFileConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if src.State() != capture.StateClosed {
		t.Fatalf("expected state closed, got %s", src.State())
	}
}

func TestContextCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.pcap")
	writeTestPcap(t, path)

	ctx, cancel := context.WithCancel(context.Background())
	src, err := capture.Open(ctx, "pcap-file", PcapFileConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	cancel()

	select {
	case <-src.Packets():
	case <-time.After(2 * time.Second):
		t.Fatal("source did not close after context cancellation")
	}
}

func TestStartTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.pcap")
	writeTestPcap(t, path)

	src, err := capture.New("pcap-file", PcapFileConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	if err := src.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := src.Start(context.Background()); !errors.Is(err, capture.ErrAlreadyStarted) {
		t.Fatalf("expected ErrAlreadyStarted, got %v", err)
	}
}

func TestValidate(t *testing.T) {
	_, err := capture.New("pcap-file", PcapFileConfig{Path: ""})
	if err == nil {
		t.Fatal("expected validation error for empty path")
	}
}

func TestReplaySpeedBurstDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.pcap")
	writeTestPcap(t, path)

	src, err := capture.Open(context.Background(), "pcap-file", PcapFileConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	start := time.Now()
	<-src.Packets()
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("default burst mode should not sleep")
	}
}
