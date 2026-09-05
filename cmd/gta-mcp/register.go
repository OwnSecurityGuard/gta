// register.go — 自助注册：完全的新用户免邀请获取独立身份（2026-09-05）。
//
// 设计：
//   - POST /access/register {"name":"carol"} → 201 {"ok":true,"owner":"carol","token":"gta_..."}
//   - 仅 token 鉴权模式下开放（匿名模式没有"用户"概念，注册无意义）；
//     GTA_AUTH_REGISTER=off 可显式关闭（封闭团队走纯邀请制）。
//   - 身份落 users 表（与邀请 claim 同表、同一条 DBResolver 解析链），即时生效。
//   - 保留名拒绝：env bootstrap 的 owner（同名会让 projects/sessions 的 owner
//     字段把两个身份混同，权限边界击穿）、匿名 owner "local"、既有邀请制用户。
//   - token 仅创建时返回一次，与邀请凭证同待遇。不做限速：部署假定内网/受信
//     网络；公网部署必须先在反代层加限流（team-deployment.md 已注明）。
package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"gta/pkg/auth"
)

type registerRequest struct {
	Name string `json:"name"`
}

type registerResponse struct {
	OK    bool   `json:"ok"`
	Owner string `json:"owner"`
	Token string `json:"token"`
}

// handleRegister 处理自助注册（鉴权豁免端点，挂在 /access/* 一组）。
func (m *mcpCapture) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !m.openRegister {
		http.Error(w, "registration is disabled on this server (token auth off or GTA_AUTH_REGISTER=off)", http.StatusForbidden)
		return
	}
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: expect {\"name\":\"...\"}", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validOwnerName(name) {
		http.Error(w, "invalid name: letters/digits/._- , starts with letter or digit, max 64 chars", http.StatusBadRequest)
		return
	}
	if name == auth.AnonymousOwner {
		http.Error(w, "name is reserved", http.StatusConflict)
		return
	}
	if m.envResolver.HasOwner(name) {
		http.Error(w, "name is reserved by server bootstrap tokens", http.StatusConflict)
		return
	}
	ctx := r.Context()
	if exists, err := m.users.OwnerExists(ctx, name); err != nil {
		http.Error(w, "lookup user: "+err.Error(), http.StatusInternalServerError)
		return
	} else if exists {
		http.Error(w, "user "+name+" already exists", http.StatusConflict)
		return
	}
	// createdBy 空 = 自主注册（区别于邀请：created_by=邀请人）。
	u, token, err := m.users.CreateUser(ctx, name, "")
	if err != nil {
		// 并发同名注册的 UNIQUE 冲突也落在这里，语义同为已存在/冲突。
		http.Error(w, "register: "+err.Error(), http.StatusConflict)
		return
	}
	slog.Info("self registration", "owner", u.Owner, "remote", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(registerResponse{OK: true, Owner: u.Owner, Token: token})
}
