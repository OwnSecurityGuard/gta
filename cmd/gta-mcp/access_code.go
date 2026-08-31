// access_code.go — 启动码 GTA-XXXX 机制：生成一个绑定 owner/项目的短码，成员在
// 目标机输入后由 agent 用 <code> 调 /access/claim 拿回完整配置（复用手动下载的
// 开会话/组 sidecar 配置逻辑，改 JSON 返回取代 zip 打包）。
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"gta/pkg/auth"
)

const accessCodeSchema = `
CREATE TABLE IF NOT EXISTS access_codes (
    code        TEXT PRIMARY KEY,
    owner       TEXT NOT NULL DEFAULT '',
    project_id  TEXT NOT NULL DEFAULT '',
    plugin      TEXT NOT NULL DEFAULT '',
    port        INTEGER NOT NULL DEFAULT 0,
    server      TEXT NOT NULL DEFAULT '',
    platform    TEXT NOT NULL DEFAULT '',
    created_at  DATETIME NOT NULL,
    expires_at  DATETIME NOT NULL,
    claimed     INTEGER NOT NULL DEFAULT 0,
    session_id  TEXT NOT NULL DEFAULT ''
);`

type accessCode struct {
	Code      string    `json:"code"`
	Owner     string    `json:"owner"`
	ProjectID string    `json:"project_id,omitempty"`
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
	_, err := s.db.Exec(accessCodeSchema)
	return err
}

func (s *accessCodeStore) Create(ctx context.Context, c *accessCode) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO access_codes(code,owner,project_id,plugin,port,server,platform,created_at,expires_at,claimed,session_id)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		c.Code, c.Owner, c.ProjectID, c.Plugin, c.Port, c.Server, c.Platform,
		c.CreatedAt.Format(time.RFC3339), c.ExpiresAt.Format(time.RFC3339),
		boolInt(c.Claimed), c.SessionID)
	return err
}

func (s *accessCodeStore) Get(ctx context.Context, code string) (*accessCode, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT code,owner,project_id,plugin,port,server,platform,created_at,expires_at,claimed,session_id
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
		`SELECT code,owner,project_id,plugin,port,server,platform,created_at,expires_at,claimed,session_id
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
	err := s.Scan(&c.Code, &c.Owner, &c.ProjectID, &c.Plugin, &c.Port, &c.Server, &c.Platform,
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

// newAccessCode 生成形如 GTA-XXXX-XXXX 的短码（大写字面 + 数字，规避易混淆字符）。
func newAccessCode() string {
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "GTA-DEAD-BEEF"
	}
	parts := make([]string, 2)
	for i := 0; i < 2; i++ {
		var s []byte
		for j := 0; j < 4; j++ {
			s = append(s, charset[int(b[i*2+j])%len(charset)])
		}
		parts[i] = string(s)
	}
	return "GTA-" + parts[0] + "-" + parts[1]
}

// handleCreateAccessCode 为当前用户生成一个启动码（可选绑项目/插件/端口/平台/回连地址）。
func (m *mcpCapture) handleCreateAccessCode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, _ := m.ownerScope(ctx)
	code := newAccessCode()
	port := handleProjectArgPort(req)
	plugin := req.GetString("plugin", "")
	platform := req.GetString("platform", "")
	projectID := req.GetString("project_id", "")
	server := strings.TrimSpace(req.GetString("server", ""))

	rec := &accessCode{
		Code:      code,
		Owner:     owner,
		ProjectID: projectID,
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
	slog.Info("access code created", "owner", owner, "code", code)
	return successResult(map[string]any{
		"code": code, "owner": owner, "project_id": projectID,
		"plugin": plugin, "port": port, "platform": platform, "expires_at": rec.ExpiresAt.Format(time.RFC3339),
	}), nil
}

// handleListAccessCodes 列出当前用户（或 admin 全部）启动码。
func (m *mcpCapture) handleListAccessCodes(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, all := m.ownerScope(ctx)
	codes, err := m.accessCodes.listForOwner(ctx, owner, all)
	if err != nil {
		return nil, err
	}
	return successResult(map[string]any{"codes": codes}), nil
}

// ownerSecret 返回某 owner 的静态 token（供 claim 烧进 agent 配置）；匿名模式下为空。
func (m *mcpCapture) ownerSecret(owner string) string {
	return m.tokensByOwner[owner]
}

// loadTokensByOwner 从 GTA_AUTH_TOKENS 解析 owner->token（"alice=gta_xxx:admin" 取 "gta_xxx"）。
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