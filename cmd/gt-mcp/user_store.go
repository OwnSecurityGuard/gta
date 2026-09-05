// user_store.go — 邀请制身份的持久化（users 表，2026-09-05 设计讨论定案）。
//
// 身份来源分层：
//   - env（GT_AUTH_TOKENS）：bootstrap / 首个 admin，启动时静态载入；
//   - users 表：邀请制发放的运行时身份，claim 时创建、revoke_user 撤销，
//     经 pkg/auth.DBResolver 即时生效，无需重启。
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"

	"gametrace/pkg/authz"
)

// userOwnerPattern 限制新用户名格式：防路径/命名空间歧义（owner 会进入
// 插件命名空间 owner/name 与会话归属），大小写字母数字与 ._-，不以符号开头。
var userOwnerPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// validOwnerName 校验用户名格式。
func validOwnerName(owner string) bool { return userOwnerPattern.MatchString(owner) }

// user 是 users 表的一行。
type user struct {
	Owner     string `json:"owner"`
	IsAdmin   bool   `json:"is_admin"`
	TenantID  string `json:"tenant_id,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at"`
}

// userStore 提供 users 表的访问（挂在 auxDB，与 projects 同库）。
type userStore struct{ db *sql.DB }

func newUserStore(db *sql.DB) *userStore { return &userStore{db: db} }

// Init 建表（幂等）。
func (us *userStore) Init() error {
	schema := `
CREATE TABLE IF NOT EXISTS users (
    owner      TEXT PRIMARY KEY,
    token      TEXT NOT NULL UNIQUE,
    tenant_id  TEXT NOT NULL DEFAULT 'default',
    is_admin   INTEGER NOT NULL DEFAULT 0,
    created_by TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL
);`
	if _, err := us.db.Exec(schema); err != nil {
		return fmt.Errorf("create users table: %w", err)
	}
	return nil
}

// newInviteToken 生成 gt_ 前缀的 192bit 随机 token（hex）。
func newInviteToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate invite token: %w", err)
	}
	return "gt_" + hex.EncodeToString(b), nil
}

// CreateUser 创建邀请制用户并返回为其生成的 token。
// owner 已存在返回错误（邀请面向新人；复用身份应直接共享既有 token）。
func (us *userStore) CreateUser(ctx context.Context, owner, createdBy string) (*user, string, error) {
	if !validOwnerName(owner) {
		return nil, "", fmt.Errorf("invalid owner name %q: letters/digits/._- , starts with letter or digit, max 64 chars", owner)
	}
	token, err := newInviteToken()
	if err != nil {
		return nil, "", err
	}
	_, err = us.db.ExecContext(ctx,
		`INSERT INTO users(owner, token, tenant_id, is_admin, created_by, created_at) VALUES (?,?,?,?,?,?)`,
		owner, token, authz.DefaultTenant, 0, createdBy, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return nil, "", fmt.Errorf("create user %s: %w", owner, err)
	}
	return &user{Owner: owner, IsAdmin: false, TenantID: authz.DefaultTenant, CreatedBy: createdBy,
		CreatedAt: time.Now().UTC().Format(time.RFC3339)}, token, nil
}

// OwnerExists 报告用户名是否已被占用（发邀请前的预检）。
func (us *userStore) OwnerExists(ctx context.Context, owner string) (bool, error) {
	var one int
	err := us.db.QueryRowContext(ctx, `SELECT 1 FROM users WHERE owner=?`, owner).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Revoke 删除用户（撤销其 token 即时失效）；返回是否删到了记录。
func (us *userStore) Revoke(ctx context.Context, owner string) (bool, error) {
	res, err := us.db.ExecContext(ctx, `DELETE FROM users WHERE owner=?`, owner)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListUsers 列出全部用户（不含 token —— 凭证只在创建时展示一次）。
func (us *userStore) ListUsers(ctx context.Context) ([]user, error) {
	rows, err := us.db.QueryContext(ctx,
		`SELECT owner, is_admin, tenant_id, created_by, created_at FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []user
	for rows.Next() {
		var u user
		var admin int
		if err := rows.Scan(&u.Owner, &admin, &u.TenantID, &u.CreatedBy, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.IsAdmin = admin != 0
		out = append(out, u)
	}
	return out, rows.Err()
}
