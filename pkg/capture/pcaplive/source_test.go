package pcaplive

import (
	"context"
	"testing"

	"gta/pkg/capture"
)

func TestOpenNonexistentDevice(t *testing.T) {
	_, err := capture.Open(context.Background(), "pcap-live", PcapLiveConfig{Device: "nonexistent0"})
	if err == nil {
		t.Fatal("expected error from nonexistent device")
	}
}

func TestValidateEmptyDeviceAllowed(t *testing.T) {
	// 空 device 现在合法：表示监听所有设备。设备解析在 setup 阶段进行。
	_, err := capture.New("pcap-live", PcapLiveConfig{Device: ""})
	if err != nil {
		t.Fatalf("expected no validation error for empty device (captures all), got: %v", err)
	}
}

func TestValidateNegativeSnapLen(t *testing.T) {
	_, err := capture.New("pcap-live", PcapLiveConfig{SnapLen: -1})
	if err == nil {
		t.Fatal("expected validation error for negative snap_len")
	}
}

func TestValidateDevicesList(t *testing.T) {
	// Devices 列表在 validate 阶段不校验存在性，setup 阶段才校验。
	_, err := capture.New("pcap-live", PcapLiveConfig{Devices: []string{"eth0", "eth1"}})
	if err != nil {
		t.Fatalf("expected no validation error for devices list, got: %v", err)
	}
}
