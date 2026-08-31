// access_code.go — 启动码 GTA-XXXX 机制：生成一个绑定 owner/项目的短码，成员在
// 目标机输入后由 agent 用 <code> 调 /access/claim 拿回完整配置（复用手动下载的
// 开会话/组 sidecar 配置逻辑，改 JSON 返回取代 zip 打包）。
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"time"
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