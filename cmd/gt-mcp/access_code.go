// access_code.go — 启动码 GT-XXXX 机制：生成一个绑定 owner/项目的短码，成员在
// 目标机输入后由 agent 用 <code> 调 /access/claim 拿回完整配置（复用手动下载的
// 开会话/组 sidecar 配置逻辑，改 JSON 返回取代 zip 打包）。
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"gametrace/pkg/auth"
	"gametrace/pkg/authz"
	pb "gametrace/pkg/internalipc/proto"
)

const accessCodeSchema = `
CREATE TABLE IF NOT EXISTS access_codes (
    code        TEXT PRIMARY KEY,
    owner       TEXT NOT NULL DEFAULT '',
    project_id  TEXT NOT NULL DEFAULT '',
    new_owner   TEXT NOT NULL DEFAULT '',
    plugin      TEXT NOT NULL DEFAULT '',
    port        INTEGER NOT NULL DEFAULT 0,
    server      TEXT NOT NULL DEFAULT '',
    platform    TEXT NOT NULL DEFAULT '',
    created_at  DATETIME NOT NULL,
    expires_at  DATETIME NOT NULL,
    claimed     INTEGER NOT NULL DEFAULT 0,
    session_id  TEXT NOT NULL DEFAULT ''
);`

// accessCodeNewOwnerCol 是邀请制（new_owner）列的幂等补列语句：
// 既有库由 Init 时 PRAGMA 复查 + ALTER 兜底（与 projects.owner 列迁移同款策略）。
const accessCodeNewOwnerDDL = `ALTER TABLE access_codes ADD COLUMN new_owner TEXT NOT NULL DEFAULT ''`

type accessCode struct {
	Code      string    `json:"code"`
	Owner     string    `json:"owner"`
	ProjectID string    `json:"project_id,omitempty"`
	// NewOwner 非空表示这是邀请码：claim 时为该名字创建独立身份（users 表），
	// 而不是把 code 创建者的身份借给目标机。
	NewOwner  string    `json:"new_owner,omitempty"`
	Plugin    string    `json:"plugin,omitempty"`
	Port      int       `json:"port"`
	Server    string    `json:"server,omitempty"`
	Platform  string    `json:"platform,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Claimed   bool      `json:"claimed"`
	SessionID string    `json:"session_id,omitempty"`
}

type accessCodeStore struct{ db *sql.DB }

func newAccessCodeStore(db *sql.DB) *accessCodeStore { return &accessCodeStore{db: db} }

func (s *accessCodeStore) Init() error {
	if _, err := s.db.Exec(accessCodeSchema); err != nil {
		return err
	}
	// 既有库补 new_owner 列（CREATE TABLE IF NOT EXISTS 不会为老表加列）。
	if !sqliteHasColumn(s.db, "access_codes", "new_owner") {
		if _, err := s.db.Exec(accessCodeNewOwnerDDL); err != nil {
			if !sqliteHasColumn(s.db, "access_codes", "new_owner") {
				return fmt.Errorf("migrate access_codes.new_owner: %w", err)
			}
		}
	}
	return nil
}

func (s *accessCodeStore) Create(ctx context.Context, c *accessCode) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO access_codes(code,owner,project_id,new_owner,plugin,port,server,platform,created_at,expires_at,claimed,session_id)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.Code, c.Owner, c.ProjectID, c.NewOwner, c.Plugin, c.Port, c.Server, c.Platform,
		c.CreatedAt.Format(time.RFC3339), c.ExpiresAt.Format(time.RFC3339),
		boolInt(c.Claimed), c.SessionID)
	return err
}

func (s *accessCodeStore) Get(ctx context.Context, code string) (*accessCode, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT code,owner,project_id,new_owner,plugin,port,server,platform,created_at,expires_at,claimed,session_id
		 FROM access_codes WHERE code=?`, code)
	return scanAccessCode(row)
}

func (s *accessCodeStore) MarkClaimed(ctx context.Context, code, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE access_codes SET claimed=1, session_id=? WHERE code=?`, sessionID, code)
	return err
}

func (s *accessCodeStore) listForOwner(ctx context.Context, owner string, all bool) ([]accessCode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT code,owner,project_id,new_owner,plugin,port,server,platform,created_at,expires_at,claimed,session_id
		 FROM access_codes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []accessCode
	for rows.Next() {
		c, err := scanAccessCode(rows)
		if err != nil {
			return nil, err
		}
		if all || c.Owner == owner {
			out = append(out, *c)
		}
	}
	return out, rows.Err()
}

func scanAccessCode(s interface{ Scan(dest ...any) error }) (*accessCode, error) {
	var c accessCode
	var ca, ea string
	var claimed int
	err := s.Scan(&c.Code, &c.Owner, &c.ProjectID, &c.NewOwner, &c.Plugin, &c.Port, &c.Server, &c.Platform,
		&ca, &ea, &claimed, &c.SessionID)
	if err != nil {
		return nil, err
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339, ca)
	c.ExpiresAt, _ = time.Parse(time.RFC3339, ea)
	c.Claimed = claimed != 0
	return &c, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// newAccessCode 生成形如 GT-XXXX-XXXX 的短码（大写字面 + 数字，规避易混淆字符）。
func newAccessCode() string {
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "GT-DEAD-BEEF"
	}
	parts := make([]string, 2)
	for i := 0; i < 2; i++ {
		var s []byte
		for j := 0; j < 4; j++ {
			s = append(s, charset[int(b[i*2+j])%len(charset)])
		}
		parts[i] = string(s)
	}
	return "GT-" + parts[0] + "-" + parts[1]
}

// handleCreateAccessCode 生成启动码（可选绑项目/插件/端口/平台/回连地址）。
// 启动码泄露即可换取 owner 的长期 token，属高敏感动作（方案 D7）：
// 只能为本人创建；绑定项目时要求对该项目有 ActionProjectRead。
//
// 邀请模式（new_owner 非空）：claim 时为 new_owner 创建**独立身份**（users 表），
// 而不是把调用者身份借给目标机 —— 新用户凭码即可获得自己的 token（2026-09-05 邀请制）。
func (m *mcpCapture) handleCreateAccessCode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	p := authzPrincipal(ctx)
	owner := p.User
	if !authz.AccessCodeActionAllowed(p, authz.ActionAccessCodeCreate, owner) {
		return errorResult(fmt.Errorf("forbidden: cannot create access code for others")), nil
	}
	code := newAccessCode()
	port := handleProjectArgPort(req)
	plugin := req.GetString("plugin", "")
	platform := req.GetString("platform", "")
	projectID := req.GetString("project_id", "")
	server := strings.TrimSpace(req.GetString("server", ""))
	// 邀请模式参数：new_owner 是将被创建的新身份名。
	newOwner := strings.TrimSpace(req.GetString("new_owner", ""))
	if newOwner != "" {
		if owner == "" {
			return errorResult(fmt.Errorf("anonymous callers cannot issue invitations; bootstrap a token first")), nil
		}
		if !validOwnerName(newOwner) {
			return errorResult(fmt.Errorf("invalid new_owner %q: letters/digits/._- , starts with letter or digit, max 64 chars", newOwner)), nil
		}
		exists, err := m.users.OwnerExists(ctx, newOwner)
		if err != nil {
			return nil, err
		}
		if exists {
			return errorResult(fmt.Errorf("user %s already exists; invitations are for new identities only", newOwner)), nil
		}
	}
	if projectID != "" {
		target, err := m.projects.Get(ctx, projectID)
		if err != nil {
			return nil, err
		}
		if target == nil {
			return errorResult(fmt.Errorf("project %s not found", projectID)), nil
		}
		if err := m.authz.Can(ctx, authz.ActionProjectRead, projectResource(target)); err != nil {
			return errorResult(fmt.Errorf("project %s not found", projectID)), nil
		}
	}

	rec := &accessCode{
		Code:      code,
		Owner:     owner,
		ProjectID: projectID,
		NewOwner:  newOwner,
		Plugin:    plugin,
		Port:      port,
		Server:    server,
		Platform:  platform,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	if err := m.accessCodes.Create(ctx, rec); err != nil {
		return nil, fmt.Errorf("create access code: %w", err)
	}
	slog.Info("access code created", "owner", owner, "code", code, "invite_for", newOwner)
	return successResult(map[string]any{
		"code": code, "owner": owner, "project_id": projectID, "new_owner": newOwner,
		"plugin": plugin, "port": port, "platform": platform, "expires_at": rec.ExpiresAt.Format(time.RFC3339),
		"invite": newOwner != "",
	}), nil
}

// handleListAccessCodes 列出当前用户（或 admin 全部）启动码。
func (m *mcpCapture) handleListAccessCodes(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	p := authzPrincipal(ctx)
	codes, err := m.accessCodes.listForOwner(ctx, p.User, p.IsAdmin)
	if err != nil {
		return nil, err
	}
	return successResult(map[string]any{"codes": codes}), nil
}

// ownerSecret 返回某 owner 的静态 token（供 claim 烧进 agent 配置）；匿名模式下为空。
func (m *mcpCapture) ownerSecret(owner string) string {
	return m.tokensByOwner[owner]
}

// loadTokensByOwner 从 GT_AUTH_TOKENS 解析 owner->token（"alice=gt_xxx:admin" 取 "gt_xxx"）。
// 复刻 auth.ParseTokens 的格式但不改动 auth 包（后者只暴露 token->owner 单一方向）。
func loadTokensByOwner() map[string]string {
	m := map[string]string{}
	for _, seg := range strings.Split(os.Getenv(auth.EnvTokens), ",") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		eq := strings.IndexByte(seg, '=')
		if eq < 0 {
			continue
		}
		owner := strings.TrimSpace(seg[:eq])
		tok := strings.TrimSpace(seg[eq+1:])
		if i := strings.LastIndexByte(tok, ':'); i >= 0 {
			tok = tok[:i]
		}
		if owner != "" && tok != "" {
			m[owner] = tok
		}
	}
	return m
}

// handleAccessClaim 是 agent 首启时调用的未鉴权端点：携带启动码返回完整配置
// （server/registry/ingest/token/session/plugin 等），复用手动 download 的开会话
// 与组 sidecar 配置逻辑，但不打包 zip，改 JSON 返回。它只凭 code 存在性+未过期
// 即可工作；码是一次性+24h 限时，泄露面被限制在有效期内。
func (m *mcpCapture) handleAccessClaim(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("code")))
	if code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	rec, err := m.accessCodes.Get(ctx, code)
	if err != nil || rec == nil {
		http.Error(w, "invalid access code", http.StatusNotFound)
		return
	}
	if time.Now().After(rec.ExpiresAt) {
		http.Error(w, "access code expired", http.StatusGone)
		return
	}

	// 复用 download 的开会话逻辑：从该 code 的 recipe 开 agent 接收会话。
	if m.pipelineClient == nil {
		http.Error(w, "pipeline is not reachable", http.StatusServiceUnavailable)
		return
	}
	owner := rec.Owner
	token := m.ownerSecret(owner)

	// 邀请模式：为 new_owner 创建独立身份（users 表 + 即时生效的 DB resolver），
	// 会话与回连凭证都挂在新身份名下 —— 新用户获得"自己的"token，
	// 而不是借 code 创建者的身份（2026-09-05 邀请制）。
	if rec.NewOwner != "" {
		if m.users == nil {
			http.Error(w, "invite is not available on this deployment", http.StatusServiceUnavailable)
			return
		}
		u, newToken, err := m.users.CreateUser(ctx, rec.NewOwner, rec.Owner)
		if err != nil {
			slog.Warn("invite claim: create user failed", "new_owner", rec.NewOwner, "error", err)
			http.Error(w, "invite claim failed: "+err.Error(), http.StatusConflict)
			return
		}
		owner = u.Owner
		token = newToken
		slog.Info("invite claimed: new identity created", "new_owner", owner, "created_by", rec.Owner, "code", code)
	}

	grpcReq := &pb.StartCaptureRequest{Plugin: rec.Plugin, Agent: true, Owner: owner}
	if rec.ProjectID != "" {
		grpcReq.ProjectId = rec.ProjectID
	}
	// 插件解析候选：认领者自己所属项目的插件 + 码创建者（设备码绑定的项目插件
	// 由创建者设置，认领者作为项目成员/新成员要能解析到它）。
	grpcReq.PluginOwners = m.pluginOwnersFor(ctx, owner)
	if rec.Owner != "" && rec.Owner != owner {
		grpcReq.PluginOwners = append(grpcReq.PluginOwners, rec.Owner)
	}
	gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := m.pipelineClient.StartCapture(gctx, grpcReq)
	if err != nil {
		http.Error(w, "open receive session failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	sessionID := resp.GetSessionId()
	// 回连地址与凭证：registry 取默认或 code 覆盖值；token 在上方按模式取定
	//（借身份 = code 创建者的静态凭证；邀请 = 为 new_owner 新发的独立凭证）。
	registry, ingest := m.registryIngest(ctx)
	if rec.Server != "" {
		registry = rec.Server
	}
	cfg := map[string]any{
		"server":       registry,
		"ingest_addr":  ingest,
		"token":        token,
		"session":      sessionID,
		"bpf":          accessCodeBPF(rec.Port),
		"plugin_names": accessCodePlugins(rec.Plugin),
	}

	if err := m.accessCodes.MarkClaimed(ctx, code, sessionID); err != nil {
		slog.Warn("mark access code claimed failed", "code", code, "error", err)
	}
	// 同步会话 metadata（供前端派生在线/离线与项目归属）。
	meta := sessionMetadata{
		Owner:     owner,
		SessionID: sessionID,
		StartedAt: time.Now().Format(time.RFC3339),
		Status:    "running",
		Port:      rec.Port,
		Plugin:    rec.Plugin,
		Source:    "agent",
		DBPath:    resp.GetDbPath(),
		ProjectID: rec.ProjectID,
	}
	if m.sessionMgr != nil {
		if err := m.sessionMgr.writeSessionMetadata(sessionID, meta); err != nil {
			slog.Warn("write session metadata in claim failed", "error", err)
		}
		m.sessionMgr.writeCurrent(meta)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Session-Id", sessionID)
	_ = json.NewEncoder(w).Encode(cfg)
	slog.Info("access code claimed", "owner", owner, "code", code, "session", sessionID)
}

func accessCodeBPF(port int) string {
	if port <= 0 {
		return ""
	}
	return fmt.Sprintf("tcp port %d or udp port %d", port, port)
}

func accessCodePlugins(plugin string) []string {
	if plugin == "" {
		return []string{}
	}
	return []string{plugin}
}

// handleListUsers 列出成员账号（仅 global admin；不回 token —— 凭证只在创建时展示一次）。
// users 表之外，env bootstrap 身份（GT_AUTH_TOKENS）以 bootstrap_owners 单独返回：
// 它们不在 users 表、不可撤销，但成员管理界面应可见，否则 admin 看不到自己。
func (m *mcpCapture) handleListUsers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := m.authz.Can(ctx, authz.ActionUserManage, authz.Resource{Kind: authz.KindUser}); err != nil {
		return errorResult(err), nil
	}
	users, err := m.users.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	if users == nil {
		users = []user{}
	}
	bootstrap := m.envResolver.Owners()
	if bootstrap == nil {
		bootstrap = []string{}
	}
	return successResult(map[string]any{"users": users, "bootstrap_owners": bootstrap}), nil
}

// handleRevokeUser 撤销邀请制用户（删除 users 行，token 即时失效；仅 global admin）。
// 只能撤销 users 表里的身份：env bootstrap（GT_AUTH_TOKENS）不在此列，天然不可撤销。
func (m *mcpCapture) handleRevokeUser(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := m.authz.Can(ctx, authz.ActionUserManage, authz.Resource{Kind: authz.KindUser}); err != nil {
		return errorResult(err), nil
	}
	owner := strings.TrimSpace(req.GetString("owner", ""))
	if owner == "" {
		return errorResult(fmt.Errorf("owner is required")), nil
	}
	if owner == authzPrincipal(ctx).User {
		return errorResult(fmt.Errorf("cannot revoke yourself")), nil
	}
	found, err := m.users.Revoke(ctx, owner)
	if err != nil {
		return nil, err
	}
	if !found {
		return errorResult(fmt.Errorf("user %s not found (env bootstrap tokens are not revocable here)", owner)), nil
	}
	slog.Info("user revoked", "owner", owner, "actor", authzPrincipal(ctx).User)
	return successResult(map[string]any{"owner": owner, "revoked": true}), nil
}

// baseURL 从请求回推 scheme+host，供 setup.sh 等脚本拼接 download/claim 地址。
func (m *mcpCapture) baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func linuxAmd64Platform() string { return "linux/amd64" }

// handleSetupScript 返回 `curl ... | bash` 的一键脚本：先用启动码调 /access/claim 拿
// sidecar 配置（含 token/session），再带 Bearer 下载本平台 agent zip、解压并把配置写入
// config.embedded.json，随后启动 gt-agent。code 由调用方拼在 URL。
func (m *mcpCapture) handleSetupScript(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("code")))
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	if code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}
	if platform == "" {
		platform = linuxAmd64Platform()
	}
	base := m.baseURL(r)
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
CODE="%[1]s"
PLATFORM="%[2]s"
BIN_DIR="$HOME/.gt-agent"
BASE="%[3]s"
echo "领取启动码配置（token/session）..."
CONFIG_JSON=$(curl -fsSL "$BASE/access/claim?code=$CODE") || { echo "启动码无效或已过期"; exit 1; }
TOKEN=$(printf '%%s' "$CONFIG_JSON" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
AUTH=()
if [ -n "$TOKEN" ]; then AUTH=(-H "Authorization: Bearer $TOKEN"); fi
echo "下载 Agent（%[2]s）..."
mkdir -p "$BIN_DIR"
curl -fsSL "${AUTH[@]}" "$BASE/download/agent?code=$CODE&platform=$PLATFORM" -o /tmp/gt-agent.zip || { echo "下载失败，请确认目标机可访问服务端 $BASE"; exit 1; }
unzip -o -q /tmp/gt-agent.zip -d "$BIN_DIR" || { echo "解压失败，请确认已安装 unzip"; exit 1; }
printf '%%s' "$CONFIG_JSON" > "$BIN_DIR/config.embedded.json"
chmod +x "$BIN_DIR/gt-agent"
echo "已在 $BIN_DIR 完成安装。执行 $BIN_DIR/gt-agent 开始抓包上报。"
`, code, platform, base)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(script))
	slog.Info("setup script served", "code", code, "platform", platform)
}

// handleSetupScriptPS1 返回 Windows PowerShell 一键脚本（与 /setup.sh 对称）：
// 先用启动码调 /access/claim 拿 sidecar 配置，再下载 agent zip 并解压写入配置，
// 最后启动 gt-agent.exe。支持 `irm ... | iex` 一键执行。
func (m *mcpCapture) handleSetupScriptPS1(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("code")))
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	if code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}
	if platform == "" {
		platform = "windows/amd64"
	}
	base := m.baseURL(r)
	script := fmt.Sprintf(`$ErrorActionPreference = "Stop"
$code = "%[1]s"
$platform = "%[2]s"
$base = "%[3]s"
$binDir = Join-Path $env:USERPROFILE ".gt-agent"
Write-Host "领取启动码配置（token/session）..."
$cfg = Invoke-RestMethod -Uri "$base/access/claim?code=$code"
$token = $cfg.token
$headers = @{}
if ($token) { $headers["Authorization"] = "Bearer $token" }
Write-Host "下载 Agent（$platform）..."
New-Item -ItemType Directory -Force -Path $binDir | Out-Null
$zipPath = Join-Path $env:TEMP "gt-agent.zip"
Invoke-WebRequest -Headers $headers -Uri "$base/download/agent?code=$code&platform=$platform" -OutFile $zipPath
Expand-Archive -Path $zipPath -DestinationPath $binDir -Force
$cfg | ConvertTo-Json -Compress | Set-Content (Join-Path $binDir "config.embedded.json") -Encoding utf8
$exe = Get-ChildItem -Path $binDir -Filter "gt-agent*.exe" | Select-Object -First 1
if ($exe) {
  Write-Host "已在 $binDir 完成安装。启动 $($exe.Name) 开始抓包上报..."
  Start-Process $exe.FullName
} else {
  Write-Host "已在 $binDir 完成安装。请手动运行 gt-agent.exe 开始抓包上报。"
}
`, code, platform, base)
	w.Header().Set("Content-Type", "text/x-powershell; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(script))
	slog.Info("setup.ps1 served", "code", code, "platform", platform)
}
