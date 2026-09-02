package plugin

import (
	"strings"
	"testing"
)

func TestMajorVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"v1", "v1"},
		{"v1.2", "v1"},
		{"v1.2.3", "v1"},
		{"v2.0", "v2"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := majorVersion(tt.input)
			if got != tt.want {
				t.Fatalf("majorVersion(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCheckManifestVersion(t *testing.T) {
	// 注：ProtocolVersion 是编译期常量 "v2"，无法在测试中动态修改。
	// 因此只测 v2 匹配、v1/v3 不匹配、空/非法格式 三类场景。
	// 若未来 ProtocolVersion 改为可配置变量，可补充 v2 匹配场景。
	tests := []struct {
		name    string
		apiVer  string
		wantErr bool
		errSub  string
	}{
		{
			name:    "matching v2",
			apiVer:  "gta.decoder/v2",
			wantErr: false,
		},
		{
			name:    "matching v2 with minor",
			apiVer:  "gta.decoder/v2.1",
			wantErr: false,
		},
		{
			name:    "major mismatch v1 vs v2",
			apiVer:  "gta.decoder/v1",
			wantErr: true,
			errSub:  "version mismatch",
		},
		{
			name:    "major mismatch v3 vs v2",
			apiVer:  "gta.decoder/v3",
			wantErr: true,
			errSub:  "version mismatch",
		},
		{
			name:    "empty api_version",
			apiVer:  "",
			wantErr: true,
			errSub:  "api_version",
		},
		{
			name:    "invalid format no prefix",
			apiVer:  "v1",
			wantErr: true,
			errSub:  "api_version",
		},
		{
			name:    "invalid format empty version",
			apiVer:  "gta.decoder/",
			wantErr: true,
			errSub:  "api_version",
		},
		{
			name:    "invalid format wrong service",
			apiVer:  "gta.other/v1",
			wantErr: true,
			errSub:  "api_version",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manifest{APIVersion: tt.apiVer}
			err := CheckManifestVersion(m)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errSub)
				}
				if !strings.Contains(err.Error(), tt.errSub) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.errSub)
				}
			} else {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
			}
		})
	}
}
