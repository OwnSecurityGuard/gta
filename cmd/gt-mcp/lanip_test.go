package main

import (
	"net"
	"net/netip"
	"strings"
	"testing"
)

// TestLanIP_OverrideWins 显式覆盖永远优先，且不会被启发式「修正」。
func TestLanIP_OverrideWins(t *testing.T) {
	prev := lanIPOverride
	defer func() { lanIPOverride = prev }()

	lanIPOverride = "192.168.99.42"
	if got := lanIP(); got != "192.168.99.42" {
		t.Fatalf("override not respected: got %q", got)
	}
}

// TestLanIP_OverrideRejectsLoopback 显式 127.0.0.1 应被拒绝并回退启发式
// （手机永远不会接回环）。
func TestLanIP_OverrideRejectsLoopback(t *testing.T) {
	prev := lanIPOverride
	defer func() { lanIPOverride = prev }()

	lanIPOverride = "127.0.0.1"
	// 这里我们只断言「没回写到 loopback」，不强求具体值（启发式受机器影响）。
	if got := lanIP(); got == "127.0.0.1" {
		t.Fatalf("override accepted loopback: %q", got)
	}
}

// TestIsReachableHostLANIP 验证启发式第一关：172.16/12（docker bridge 常见段）
// 与 169.254/16（link-local）、100.64/10（CGNAT）必须被剔除。
func TestIsReachableHostLANIP(t *testing.T) {
	reject := []string{
		// docker bridge 默认段（RFC1918 私网，子集但容器常占用）
		"172.17.0.1",
		"172.18.0.1",
		"172.19.0.10",
		"172.31.255.254",
		// link-local
		"169.254.1.1",
		// CGNAT
		"100.64.0.1",
		"100.127.255.254",
		// loopback
		"127.0.0.1",
	}
	for _, ip := range reject {
		if isReachableHostLANIP(net.ParseIP(ip)) {
			t.Errorf("expected reject: %s", ip)
		}
	}
	accept := []string{
		// 10/8（典型 LAN）
		"10.0.0.1",
		// 192.168/16（典型 LAN）
		"192.168.1.10",
		"192.168.50.1",
	}
	for _, ip := range accept {
		if !isReachableHostLANIP(net.ParseIP(ip)) {
			t.Errorf("expected accept: %s", ip)
		}
	}
}

// TestIsWindowsHostInterfaceName 验证虚拟/容器网卡关键字都被剔除。
// 用户的宿主机物理网卡名因机器而异，但「全部剔除可疑虚拟」+「未知则放行」
// 的策略意味着 LAN 探测不会误选 docker/hyperv/wsl。
func TestIsWindowsHostInterfaceName(t *testing.T) {
	reject := []string{
		// docker（Windows 上典型名为 "vEthernet (DockerNAT)" 或 "DockerNAT"）
		"DockerNAT",
		"vEthernet (DockerNAT)",
		// Hyper-V 默认虚拟交换机
		"vEthernet (Default Switch)",
		"vEthernet (nat)",
		// WSL 桥接
		"vEthernet (WSL)",
		// VMware / VirtualBox
		"VMware Network Adapter VMnet1",
		"VirtualBox Host-Only Ethernet Adapter",
	}
	for _, name := range reject {
		if isWindowsHostInterfaceName(name) {
			t.Errorf("expected reject: %q", name)
		}
	}
	accept := []string{
		// 典型物理/无线网卡名（不同厂商各异）
		"Intel(R) Wi-Fi 6 AX201 160MHz",
		"Realtek PCIe GbE Family Controller",
		"Qualcomm Atheros QCA9377 Wireless Network Adapter",
		// 测试环境 loopback（虽然在 lanIPFromInterfaces 里会先被 FlagLoopback 排除，
		// 但本函数独立测时也保持放行，避免双重过滤）
		"loopback0",
	}
	for _, name := range accept {
		if !isWindowsHostInterfaceName(name) {
			t.Errorf("expected accept: %q", name)
		}
	}
}

// TestLanIPFromInterfaces_FiltersVirtual 网卡枚举时应用了接口名过滤：
// docker bridge 私有 IP（172.18.0.x）和 hyper-v 私有 IP（10.0.0.x 之类假数据）
// 不应被返回。
//
// 这里只构造一个「过滤是否生效」的最小用例：通过 isWindowsHostInterfaceName
// 的钩子暂时让所有虚拟名都被拒绝；之后只要这块契约稳了，
// lanIPFromInterfaces 的代码就能信任它。
func TestLanIPFromInterfaces_FiltersVirtual(t *testing.T) {
	// 保存并恢复钩子；无关测试它实际值如何演化。
	prev := isWindowsHostInterfaceName
	t.Cleanup(func() { isWindowsHostInterfaceName = prev })

	// 把钩子设为「一律拒绝」——只要 enumerate 还能跑就不会报错。
	isWindowsHostInterfaceName = func(string) bool { return false }
	if got := lanIPFromInterfaces(); got != "" {
		t.Fatalf("expected empty when all interfaces filtered, got %q", got)
	}

	// 把钩子设为「一律接受」——则取的是 enumerate 顺序里的第一个私有 IP。
	isWindowsHostInterfaceName = func(string) bool { return true }
	if got := lanIPFromInterfaces(); got != "" {
		// 至少要是合法 IPv4
		if _, err := netip.ParseAddr(got); err != nil {
			t.Fatalf("returned non-IP: %q (%v)", got, err)
		}
		if !strings.Contains(got, ".") {
			t.Fatalf("not IPv4: %q", got)
		}
	}
}
