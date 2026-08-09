package decode

import (
	"strings"
	"testing"
)

func TestExtractFields_ValidationErrors(t *testing.T) {
	tests := []struct {
		name      string
		json      string
		wantErr   bool
		errSub    string
		checkFunc func(t *testing.T, f ExtractedFields)
	}{
		{
			name:    "valid fields no error",
			json:    `{"type":"request","_fields":{"direction":"client_to_server","msg_name":"LoginReq","is_push":false}}`,
			wantErr: false,
			checkFunc: func(t *testing.T, f ExtractedFields) {
				if f.Direction != "client_to_server" {
					t.Errorf("Direction = %q", f.Direction)
				}
				if !f.HasDirection {
					t.Error("HasDirection should be true")
				}
			},
		},
		{
			name:    "invalid direction value",
			json:    `{"_fields":{"direction":"invalid_dir"}}`,
			wantErr: true,
			errSub:  "direction has invalid value",
			checkFunc: func(t *testing.T, f ExtractedFields) {
				// direction 非法时不设置 HasDirection
				if f.HasDirection {
					t.Error("HasDirection should be false for invalid direction")
				}
			},
		},
		{
			name:    "direction wrong type",
			json:    `{"_fields":{"direction":123}}`,
			wantErr: true,
			errSub:  "direction must be string",
		},
		{
			name:    "is_push wrong type",
			json:    `{"_fields":{"is_push":"yes"}}`,
			wantErr: true,
			errSub:  "is_push must be bool",
		},
		{
			name:    "msg_name wrong type",
			json:    `{"_fields":{"msg_name":123}}`,
			wantErr: true,
			errSub:  "msg_name must be string",
		},
		{
			name:    "tcp_flags wrong type",
			json:    `{"_fields":{"tcp_flags":123}}`,
			wantErr: true,
			errSub:  "tcp_flags must be string",
		},
		{
			name:    "multiple errors",
			json:    `{"_fields":{"direction":123,"is_push":"yes"}}`,
			wantErr: true,
			errSub:  "direction must be string",
		},
		{
			name:    "_fields not object",
			json:    `{"_fields":"not object"}`,
			wantErr: true,
			errSub:  "_fields is not an object",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, cleanJSON, err := ExtractFields([]byte(tt.json))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errSub) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.errSub)
				}
			} else {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
			}
			// cleanJSON 必须不含 _fields（即使校验失败）
			if len(cleanJSON) > 0 && strings.Contains(string(cleanJSON), `"_fields"`) {
				t.Errorf("cleanJSON still contains _fields: %s", string(cleanJSON))
			}
			if tt.checkFunc != nil {
				tt.checkFunc(t, f)
			}
		})
	}
}

func TestExtractFields_EmptyAndNoFields(t *testing.T) {
	// 空 JSON
	_, _, err := ExtractFields([]byte(""))
	if err != nil {
		t.Fatalf("empty json should not error: %v", err)
	}
	// 无 _fields
	_, _, err = ExtractFields([]byte(`{"type":"request"}`))
	if err != nil {
		t.Fatalf("json without _fields should not error: %v", err)
	}
}
