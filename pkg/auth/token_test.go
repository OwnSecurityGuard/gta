package auth

import (
	"testing"
)

// TestParseTokens_EmptyMeansAnonymous 验证回归底线：没有配置任何 token 时放行一切请求，
// 且身份统一为 "local"，保证现有的个人单机用法不会因为引入鉴权而跑不起来。
func TestParseTokens_EmptyMeansAnonymous(t *testing.T) {
	t.Parallel()
	r, err := ParseTokens("")
	if err != nil {
		t.Fatalf("空配置不应报错: %v", err)
	}
	if r.Required() {
		t.Fatal("空配置应处于匿名模式，Required() 必须为 false")
	}
	for _, tok := range []string{"", "gta_whatever", "Bearer gta_whatever"} {
		p, ok := r.Resolve(tok)
		if !ok {
			t.Fatalf("匿名模式下任意凭证都应放行，token=%q 被拒", tok)
		}
		if p.Owner != AnonymousOwner {
			t.Fatalf("匿名模式 owner 应为 %q，实际 %q", AnonymousOwner, p.Owner)
		}
	}
}

// TestParseTokens_ResolvesOwner 验证核心能力：token → owner 映射。
func TestParseTokens_ResolvesOwner(t *testing.T) {
	t.Parallel()
	r, err := ParseTokens("alice=gta_aaa,bob=gta_bbb")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !r.Required() {
		t.Fatal("配置了 token 后 Required() 必须为 true")
	}
	p, ok := r.Resolve("gta_aaa")
	if !ok {
		t.Fatal("正确的 token 应解析成功")
	}
	if p.Owner != "alice" {
		t.Fatalf("owner 应为 alice，实际 %q", p.Owner)
	}
	if p.IsAdmin {
		t.Fatal("未带 :admin 后缀的 token 不应是 admin")
	}
	if p, ok := r.Resolve("gta_bbb"); !ok || p.Owner != "bob" {
		t.Fatalf("bob 的 token 解析异常: %+v ok=%v", p, ok)
	}
}

// TestParseTokens_RejectsUnknown 验证错误 token 不会凑巧命中。
func TestParseTokens_RejectsUnknown(t *testing.T) {
	t.Parallel()
	r, err := ParseTokens("alice=gta_aaa")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for _, tok := range []string{"", "gta_bbb", "GTA_AAA", "gta_aaa ", "Bearer gta_aaa"} {
		if _, ok := r.Resolve(tok); ok {
			t.Fatalf("token %q 不应解析成功", tok)
		}
	}
}

// TestParseTokens_AdminSuffix 验证 "owner=token:admin" 语法。
func TestParseTokens_AdminSuffix(t *testing.T) {
	t.Parallel()
	r, err := ParseTokens("alice=gta_aaa:admin,bob=gta_bbb")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	p, ok := r.Resolve("gta_aaa")
	if !ok {
		t.Fatal("admin 的 token 应解析成功")
	}
	if !p.IsAdmin {
		t.Fatal("带 :admin 后缀的 owner 应标记为 admin")
	}
	if p.Owner != "alice" {
		t.Fatalf("owner 应为 alice，实际 %q", p.Owner)
	}
	if p2, _ := r.Resolve("gta_bbb"); p2.IsAdmin {
		t.Fatal("bob 不应是 admin")
	}
}

// TestParseTokens_InvalidFormats 验证格式错误返回明确错误而不是 panic 或静默忽略。
// 静默忽略尤其危险：管理员以为配了 admin，实际没有，排障成本极高。
func TestParseTokens_InvalidFormats(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"缺等号", "alice"},
		{"空 owner", "=gta_aaa"},
		{"空 token", "alice="},
		{"重复 owner", "alice=gta_aaa,alice=gta_bbb"},
		{"重复 token", "alice=gta_aaa,bob=gta_aaa"},
		{"未知的冒号后缀", "alice=gta_aaa:root"},
		{"空 owner 带 admin", "=gta_aaa:admin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r, err := ParseTokens(c.in)
			if err == nil {
				t.Fatalf("输入 %q 应返回错误，实际成功: %+v", c.in, r)
			}
		})
	}
}

// TestParseTokens_ToleratesWhitespace 验证配置里的空格不会破坏解析（人工编辑 env 时很常见）。
func TestParseTokens_ToleratesWhitespace(t *testing.T) {
	t.Parallel()
	r, err := ParseTokens(" alice = gta_aaa , bob=gta_bbb ")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p, ok := r.Resolve("gta_aaa"); !ok || p.Owner != "alice" {
		t.Fatalf("alice 解析异常: %+v ok=%v", p, ok)
	}
	if p, ok := r.Resolve("gta_bbb"); !ok || p.Owner != "bob" {
		t.Fatalf("bob 解析异常: %+v ok=%v", p, ok)
	}
}

// TestLoadFromEnv 验证环境变量入口：空值走匿名模式，有值正常解析，格式错误向外报错。
func TestLoadFromEnv(t *testing.T) {
	t.Run("空环境变量走匿名模式", func(t *testing.T) {
		t.Setenv(EnvTokens, "")
		r, err := LoadFromEnv()
		if err != nil {
			t.Fatalf("空 env 不应报错: %v", err)
		}
		if r.Required() {
			t.Fatal("空 env 应处于匿名模式")
		}
		if p, ok := r.Resolve("anything"); !ok || p.Owner != AnonymousOwner {
			t.Fatalf("匿名模式应放行并标记 local: %+v ok=%v", p, ok)
		}
	})

	t.Run("未设置环境变量等同于空", func(t *testing.T) {
		t.Setenv(EnvTokens, "  ")
		r, err := LoadFromEnv()
		if err != nil {
			t.Fatalf("空白 env 不应报错: %v", err)
		}
		if r.Required() {
			t.Fatal("空白 env 应处于匿名模式")
		}
	})

	t.Run("正常解析", func(t *testing.T) {
		t.Setenv(EnvTokens, "alice=gta_aaa,bob=gta_bbb:admin")
		r, err := LoadFromEnv()
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if p, ok := r.Resolve("gta_aaa"); !ok || p.Owner != "alice" || p.IsAdmin {
			t.Fatalf("alice 解析异常: %+v ok=%v", p, ok)
		}
		if p, ok := r.Resolve("gta_bbb"); !ok || p.Owner != "bob" || !p.IsAdmin {
			t.Fatalf("bob 解析异常: %+v ok=%v", p, ok)
		}
	})

	t.Run("格式错误返回错误", func(t *testing.T) {
		t.Setenv(EnvTokens, "alice")
		if _, err := LoadFromEnv(); err == nil {
			t.Fatal("格式错误应返回 error")
		}
	})
}
