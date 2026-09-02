package decode

import (
	"net/netip"
	"testing"
)

func TestFlowIDFromEndpoints_DirectionAgnostic(t *testing.T) {
	// 验证方向无关：(A,B) 与 (B,A) 返回相同值
	id1 := FlowIDFromEndpoints("1.2.3.4:5000", "5.6.7.8:8080", "tcp")
	id2 := FlowIDFromEndpoints("5.6.7.8:8080", "1.2.3.4:5000", "tcp")
	if id1 != id2 {
		t.Errorf("direction agnostic failed: %d != %d", id1, id2)
	}

	// 不同五元组返回不同值
	id3 := FlowIDFromEndpoints("1.2.3.4:5000", "5.6.7.8:9090", "tcp")
	if id1 == id3 {
		t.Errorf("different endpoints should have different flow_id")
	}

	// 不同 protocol 返回不同值
	id4 := FlowIDFromEndpoints("1.2.3.4:5000", "5.6.7.8:8080", "udp")
	if id1 == id4 {
		t.Errorf("different protocol should have different flow_id")
	}
}

func TestStripReqRespSuffix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"BuildingUpgradeReq", "BuildingUpgrade"},
		{"BuildingUpgradeResp", "BuildingUpgrade"},
		{"LoginRequest", "Login"},
		{"LoginResponse", "Login"},
		{"Heartbeat", "Heartbeat"}, // 无后缀
		{"", ""},
	}
	for _, tt := range tests {
		got := StripReqRespSuffix(tt.input)
		if got != tt.want {
			t.Errorf("StripReqRespSuffix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestInferDirectionFromJSON_HTTP(t *testing.T) {
	tests := []struct {
		json string
		want string
	}{
		{`{"data":{"type":"request","method":"POST"}}`, "client_to_server"},
		{`{"data":{"type":"response","status":"200"}}`, "server_to_client"},
		{`{"data":{"foo":"bar"}}`, ""}, // 非 HTTP
		{``, ""},                       // 空
		{`invalid`, ""},                // 非法 JSON
		{`{"foo":"bar"}`, ""},          // 无 data 键
	}
	for _, tt := range tests {
		got := InferDirectionFromJSON([]byte(tt.json))
		if got != tt.want {
			t.Errorf("InferDirectionFromJSON(%q) = %q, want %q", tt.json, got, tt.want)
		}
	}
}

func TestInferMsgNameFromJSON_HTTP(t *testing.T) {
	tests := []struct {
		json string
		want string
	}{
		{`{"data":{"type":"request","method":"POST","path":"/api/login"}}`, "POST /api/login"},
		{`{"data":{"type":"response","status":"200"}}`, "resp 200"},
		{`{"data":{"foo":"bar"}}`, ""},
		{``, ""},
	}
	for _, tt := range tests {
		got := InferMsgNameFromJSON([]byte(tt.json))
		if got != tt.want {
			t.Errorf("InferMsgNameFromJSON(%q) = %q, want %q", tt.json, got, tt.want)
		}
	}
}

// netipMustParse 解析地址，失败时 panic（仅测试用）。
func netipMustParse(s string) netip.AddrPort {
	addr, err := netip.ParseAddrPort(s)
	if err != nil {
		panic(err)
	}
	return addr
}
