package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"gametrace/pkg/auth"
	"gametrace/pkg/store"
)

// newRegisterMCP 构造仅装配注册路径依赖的 mcpCapture（users 表 + env resolver）。
func newRegisterMCP(t *testing.T, envSpec string) *mcpCapture {
	t.Helper()
	env, err := auth.ParseTokens(envSpec)
	if err != nil {
		t.Fatalf("parse env tokens: %v", err)
	}
	cs, err := store.NewControlStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	us := newUserStore(cs.DB())
	if err := us.Init(); err != nil {
		t.Fatal(err)
	}
	return &mcpCapture{users: us, envResolver: env}
}

func doRegister(m *mcpCapture, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/access/register", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	m.handleRegister(rec, req)
	return rec
}

// TestRegister_HappyPath：新用户注册 → 201 + 独立 token，二次注册同名冲突。
func TestRegister_HappyPath(t *testing.T) {
	m := newRegisterMCP(t, "")
	m.openRegister = true

	rec := doRegister(m, `{"name":"carol"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var res registerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Owner != "carol" || !strings.HasPrefix(res.Token, "gt_") {
		t.Fatalf("unexpected response: %+v", res)
	}
	// created_by 应为空（自主注册，区别于邀请）
	exists, err := m.users.OwnerExists(t.Context(), "carol")
	if err != nil || !exists {
		t.Fatalf("carol should exist: %v %v", exists, err)
	}

	// 同名再注册 → 409
	if rec := doRegister(m, `{"name":"carol"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate register status = %d", rec.Code)
	}
}

// TestRegister_ReservedNames：env bootstrap owner、匿名 owner、非法名字一律拒绝。
func TestRegister_ReservedNames(t *testing.T) {
	m := newRegisterMCP(t, "bob=gt_tok_change_me2:admin")
	m.openRegister = true

	cases := []struct {
		name string
		body string
		want int
	}{
		{"env owner 名保留", `{"name":"bob"}`, http.StatusConflict},
		{"匿名 owner 保留", `{"name":"local"}`, http.StatusConflict},
		{"非法格式", `{"name":"1bad name"}`, http.StatusBadRequest},
		{"空名", `{"name":"  "}`, http.StatusBadRequest},
		{"坏 JSON", `{name}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		if rec := doRegister(m, c.body); rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d (body=%s)", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
}

// TestRegister_Disabled：开关关闭（或匿名模式）时 403。
func TestRegister_Disabled(t *testing.T) {
	m := newRegisterMCP(t, "")
	m.openRegister = false
	if rec := doRegister(m, `{"name":"carol"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("disabled register status = %d", rec.Code)
	}
}
