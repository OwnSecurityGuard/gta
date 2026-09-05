package auth

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func newUsersDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", t.TempDir()+"/u.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE users (
		owner TEXT PRIMARY KEY, token TEXT NOT NULL UNIQUE,
		tenant_id TEXT NOT NULL DEFAULT 'default',
		is_admin INTEGER NOT NULL DEFAULT 0,
		created_by TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedUser(t *testing.T, db *sql.DB, owner, token string, admin bool) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO users(owner, token, is_admin, created_at) VALUES (?,?,?,datetime('now'))`,
		owner, token, boolInt(admin))
	if err != nil {
		t.Fatal(err)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestDBResolver 验证 users 表精确匹配与 admin/tenant 透传。
func TestDBResolver(t *testing.T) {
	db := newUsersDB(t)
	seedUser(t, db, "bob", "tok-bob", false)
	seedUser(t, db, "root", "tok-root", true)
	r := NewDBResolver(db)

	p, ok := r.Resolve("tok-bob")
	if !ok || p.Owner != "bob" || p.IsAdmin {
		t.Fatalf("bob resolve: %v %v", p, ok)
	}
	p, ok = r.Resolve("tok-root")
	if !ok || !p.IsAdmin {
		t.Fatalf("root resolve: %v %v", p, ok)
	}
	if _, ok := r.Resolve("nope"); ok {
		t.Fatal("unknown token must fail")
	}
	if _, ok := r.Resolve(""); ok {
		t.Fatal("empty token must fail")
	}
}

// TestFirstResolverMatrix 覆盖组合语义的四种部署形态。
func TestFirstResolverMatrix(t *testing.T) {
	envWithToken := mustStatic(t, "alice=gta_a:admin")
	envEmpty := mustStatic(t, "")

	t.Run("env only", func(t *testing.T) {
		r := NewFirstResolver(envWithToken, NewDBResolver(newUsersDB(t)))
		if !r.Required() {
			t.Fatal("env token means required")
		}
		if p, ok := r.Resolve("gta_a"); !ok || !p.IsAdmin || p.Owner != "alice" {
			t.Fatalf("env resolve: %v %v", p, ok)
		}
		if _, ok := r.Resolve("other"); ok {
			t.Fatal("unknown token must fail")
		}
	})

	t.Run("db only", func(t *testing.T) {
		db := newUsersDB(t)
		seedUser(t, db, "bob", "tok-bob", false)
		r := NewFirstResolver(envEmpty, NewDBResolver(db))
		if !r.Required() {
			t.Fatal("non-empty users table means required")
		}
		if p, ok := r.Resolve("tok-bob"); !ok || p.Owner != "bob" {
			t.Fatalf("db resolve: %v %v", p, ok)
		}
		if _, ok := r.Resolve("gta_a"); ok {
			t.Fatal("env empty: env tokens must not resolve")
		}
	})

	t.Run("both empty is anonymous", func(t *testing.T) {
		r := NewFirstResolver(envEmpty, NewDBResolver(newUsersDB(t)))
		if r.Required() {
			t.Fatal("empty sources = anonymous")
		}
		p, ok := r.Resolve("whatever")
		if !ok || p.Owner != AnonymousOwner {
			t.Fatalf("anonymous fallback: %v %v", p, ok)
		}
	})

	t.Run("env takes precedence over db", func(t *testing.T) {
		db := newUsersDB(t)
		seedUser(t, db, "bob", "tok-bob", false)
		r := NewFirstResolver(envWithToken, NewDBResolver(db))
		if p, ok := r.Resolve("gta_a"); !ok || p.Owner != "alice" {
			t.Fatalf("env precedence: %v %v", p, ok)
		}
		if p, ok := r.Resolve("tok-bob"); !ok || p.Owner != "bob" {
			t.Fatalf("db fallback: %v %v", p, ok)
		}
	})
}

// TestFirstResolverEmptyDBNoDeadlock 验证"env 空 + users 表空"不锁死（匿名放行），
// 以及"env 空 + users 表有用户"时无效 token 返回 401 语义。
func TestFirstResolverEmptyDBNoDeadlock(t *testing.T) {
	r := NewFirstResolver(mustStatic(t, ""), NewDBResolver(newUsersDB(t)))
	if r.Required() {
		t.Fatal("must stay anonymous")
	}
}

func mustStatic(t *testing.T, spec string) *StaticResolver {
	t.Helper()
	r, err := ParseTokens(spec)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
