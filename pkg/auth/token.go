// Package auth 提供 GTA 从「个人单机」走向「团队共享」所需的最小身份机制：
// 静态 token → owner（用户名）。
//
// 设计要点：
//   - 匿名模式是回归底线。GTA_AUTH_TOKENS 为空时放行所有请求，身份统一为 "local"，
//     现有单机用法不会因引入鉴权而跑不起来。
//   - 本包只做「识别身份」，不做授权决策（谁能看到什么），授权留给后续任务按 owner 实现。
//   - 不做 token 签发/轮换/持久化：团队规模下静态配置足够，复杂的会引入运维成本。
package auth

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// EnvTokens 是 token 表的环境变量名，格式 "alice=gta_xxx,bob=gta_yyy"。
const EnvTokens = "GTA_AUTH_TOKENS"

// AnonymousOwner 是匿名模式下的统一 owner。
// 之所以叫 local 而不是 anonymous：它会成为插件命名空间与会话归属的一部分，
// 单机用户看到 "local/my-plugin" 比 "anonymous/my-plugin" 更符合直觉。
const AnonymousOwner = "local"

// adminSuffix 是 token 后缀形式的 admin 标记，写作 "alice=gta_xxx:admin"。
//
// 选它而不是另开一份 GTA_AUTH_ADMINS 环境变量，是因为：
//   - owner 与权限写在一处，不会漏配（漏配 admin 的表现是莫名其妙的 403，极难排障）；
//   - 团队规模下 admin 只有个位数，再引入一份 env 属于过早设计。
const adminSuffix = "admin"

// Principal 是一次请求的身份。后续任务用它做插件命名空间（owner/name）和会话隔离。
type Principal struct {
	Owner   string
	IsAdmin bool
	// Tenant 是调用者所属租户。当前 token 格式不带组织信息，恒为空串
	//（鉴权层归一为 authz.DefaultTenant）；字段先行，多租户实体后补。
	Tenant string
}

// Resolver 把凭证字符串解析成身份。接口故意只留一个方法：
// 匿名与否是 Resolve 的内部决定，调用方（拦截器/中间件）不需要分支。
type Resolver interface {
	Resolve(token string) (*Principal, bool)
}

// StaticResolver 用一次性载入的内存映射表做解析，运行期不可变，因此并发安全。
type StaticResolver struct {
	// byToken 是 token → 身份的映射；空表代表匿名模式。
	byToken map[string]Principal
}

// NewStaticResolver 用现成的映射表构造 resolver。
// 传入空表（或 nil）即匿名模式，与环境变量为空等价。
func NewStaticResolver(pairs map[string]Principal) *StaticResolver {
	byToken := make(map[string]Principal, len(pairs))
	for tok, p := range pairs {
		if tok == "" || p.Owner == "" {
			continue // 静默跳过无意义的条目，避免造出一个永远无法通过校验的死配置
		}
		byToken[tok] = p
	}
	return &StaticResolver{byToken: byToken}
}

// Required 报告是否启用了鉴权。false 表示匿名模式：任何请求都放行。
func (r *StaticResolver) Required() bool {
	return r != nil && len(r.byToken) > 0
}

// HasOwner 报告 env bootstrap 配置里是否存在指定 owner 名（与 token 值无关）。
// 用于自助注册时的保留名检查：env 身份与邀请制身份绝不能同名，否则 projects/
// sessions 的 owner 字段会把两个身份混同，权限边界直接击穿。
func (r *StaticResolver) HasOwner(name string) bool {
	if r == nil {
		return false
	}
	for _, p := range r.byToken {
		if p.Owner == name {
			return true
		}
	}
	return false
}

// Owners 返回 env bootstrap 配置的全部 owner 名（去重、升序）。
// 用于成员管理界面展示 bootstrap 身份（只读、不可撤销——它们不在 users 表）。
func (r *StaticResolver) Owners() []string {
	if r == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(r.byToken))
	out := make([]string, 0, len(r.byToken))
	for _, p := range r.byToken {
		if _, ok := seen[p.Owner]; ok {
			continue
		}
		seen[p.Owner] = struct{}{}
		out = append(out, p.Owner)
	}
	sort.Strings(out)
	return out
}

// Resolve 解析 token。
// 匿名模式下永远返回 local 身份；否则只有精确命中映射表才成功。
// 返回值是副本，调用方改动它不会影响 resolver 内部状态。
func (r *StaticResolver) Resolve(token string) (*Principal, bool) {
	if !r.Required() {
		return &Principal{Owner: AnonymousOwner}, true
	}
	p, ok := r.byToken[token]
	if !ok {
		return nil, false
	}
	return &Principal{Owner: p.Owner, IsAdmin: p.IsAdmin}, true
}

// LoadFromEnv 从 GTA_AUTH_TOKENS 载入 token 表。
// 变量缺失或为空是合法状态（匿名模式），返回可用的 resolver 而不是错误。
func LoadFromEnv() (*StaticResolver, error) {
	r, err := ParseTokens(os.Getenv(EnvTokens))
	if err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", EnvTokens, err)
	}
	return r, nil
}

// ParseTokens 解析 "owner=token,owner=token:admin" 形式的配置串。
// 格式错误一律返回错误：鉴权配置配错却静默降级成匿名模式，等于把内网服务裸奔出去。
func ParseTokens(spec string) (*StaticResolver, error) {
	if strings.TrimSpace(spec) == "" {
		return &StaticResolver{}, nil // 匿名模式
	}
	byToken := map[string]Principal{}
	owners := map[string]string{} // owner → 原始片段，用于报错时指出冲突双方
	for _, seg := range strings.Split(spec, ",") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue // 容忍尾随逗号
		}
		eq := strings.IndexByte(seg, '=')
		if eq < 0 {
			return nil, fmt.Errorf("片段 %q 缺少 '='，应为 owner=token 形式", seg)
		}
		owner := strings.TrimSpace(seg[:eq])
		if owner == "" {
			return nil, fmt.Errorf("片段 %q 的 owner 为空", seg)
		}
		token := strings.TrimSpace(seg[eq+1:])
		isAdmin := false
		if i := strings.LastIndexByte(token, ':'); i >= 0 {
			suffix := token[i+1:]
			if !strings.EqualFold(suffix, adminSuffix) {
				return nil, fmt.Errorf("片段 %q 的 token 后缀 :%s 无法识别，目前只支持 :%s", seg, suffix, adminSuffix)
			}
			token, isAdmin = token[:i], true
		}
		if token == "" {
			return nil, fmt.Errorf("片段 %q 的 token 为空", seg)
		}
		if prev, dup := owners[owner]; dup {
			return nil, fmt.Errorf("owner %q 重复定义（%q 与 %q），同一 owner 只能有一个 token", owner, prev, seg)
		}
		if other, dup := byToken[token]; dup {
			return nil, fmt.Errorf("token %q 重复定义，同时归属 %q 和 %q，会导致身份歧义", token, other.Owner, owner)
		}
		owners[owner] = seg
		byToken[token] = Principal{Owner: owner, IsAdmin: isAdmin}
	}
	return &StaticResolver{byToken: byToken}, nil
}

// parseBearer 从 Authorization 的值里取出 token。
// scheme 大小写不敏感（RFC 7235 规定）。没有 scheme 前缀时按裸 token 处理，
// 因为命令行里 -H "authorization: gta_xxx" 是很常见的写法，多容忍一种不增加复杂度。
func parseBearer(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	if i := strings.IndexByte(value, ' '); i >= 0 {
		scheme, rest := value[:i], strings.TrimSpace(value[i+1:])
		if !strings.EqualFold(scheme, "bearer") || rest == "" {
			return "", false
		}
		return rest, true
	}
	return value, true
}
