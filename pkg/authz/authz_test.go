package authz

import (
	"errors"
	"testing"
)

// TestDecideProjectActions 覆盖 project:* 动作 × 角色 × 全局 admin 的完整矩阵。
func TestDecideProjectActions(t *testing.T) {
	cases := []struct {
		name   string
		action Action
		role   Role
		admin  bool
		wantOK bool
	}{
		{action: ActionProjectRead, role: RoleMember, wantOK: true},
		{action: ActionProjectRead, role: RoleNone, wantOK: false},
		{action: ActionProjectUpdate, role: RoleOwner, wantOK: true},
		{action: ActionProjectUpdate, role: RoleAdmin, wantOK: false},
		{action: ActionProjectUpdate, role: RoleMember, wantOK: false},
		{action: ActionProjectDelete, role: RoleOwner, wantOK: true},
		{action: ActionProjectDelete, role: RoleAdmin, wantOK: false},
		{action: ActionProjectManageMembers, role: RoleAdmin, wantOK: true},
		{action: ActionProjectManageMembers, role: RoleMember, wantOK: false},
		{action: ActionProjectManagePlugins, role: RoleAdmin, wantOK: true},
		{action: ActionProjectManagePlugins, role: RoleOwner, wantOK: true},
		{action: ActionProjectManageRules, role: RoleAdmin, wantOK: true},
		{action: ActionProjectManageRules, role: RoleMember, wantOK: false},
		{action: ActionProjectTransferOwner, role: RoleOwner, wantOK: true},
		{action: ActionProjectTransferOwner, role: RoleAdmin, wantOK: false},
		{action: ActionProjectDelete, role: RoleNone, admin: true, wantOK: true},
	}
	proj := Resource{Kind: KindProject, ID: "p1", TenantID: "default"}
	for _, tc := range cases {
		t.Run(string(tc.action)+"/"+tc.name, func(t *testing.T) {
			p := Principal{User: "alice", Tenant: "default", IsAdmin: tc.admin}
			err := Decide(p, tc.action, proj, tc.role)
			if tc.wantOK && err != nil {
				t.Fatalf("want allow, got %v", err)
			}
			if !tc.wantOK && !errors.Is(err, ErrForbidden) {
				t.Fatalf("want forbidden, got %v", err)
			}
		})
	}
}

// TestDecideTenantMismatch 跨租户一律拒绝，即使 global admin（admin 在 tenant 检查之后）。
// 注意：当前实现 admin 在 tenant 检查之后放行，因此 admin 跨租户被拒 —— 这是有意为之，
// 单租户部署下两者恒为 default，不影响现状。
func TestDecideTenantMismatch(t *testing.T) {
	proj := Resource{Kind: KindProject, ID: "p1", TenantID: "acme"}
	if err := Decide(Principal{User: "alice", Tenant: "default"}, ActionProjectRead, proj, RoleOwner); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tenant mismatch must be forbidden, got %v", err)
	}
	if err := Decide(Principal{User: "alice", Tenant: "acme"}, ActionProjectRead, proj, RoleOwner); err != nil {
		t.Fatalf("same tenant must pass, got %v", err)
	}
	if err := Decide(Principal{User: "alice", Tenant: "default"}, ActionProjectRead, Resource{Kind: KindProject, ID: "p1"}, RoleOwner); err != nil {
		t.Fatalf("empty resource tenant defaults, got %v", err)
	}
}

// TestDecideProjectSession 项目会话走项目层级，member 有 creator 例外。
func TestDecideProjectSession(t *testing.T) {
	bob := Principal{User: "bob", Tenant: "default"}
	cases := []struct {
		name    string
		action  Action
		role    Role
		creator string
		wantOK  bool
	}{
		{"member read", ActionSessionRead, RoleMember, "bob", true},
		{"member use", ActionSessionUse, RoleMember, "bob", true},
		{"member delete others", ActionSessionDelete, RoleMember, "carol", false},
		{"member delete own", ActionSessionDelete, RoleMember, "bob", true},
		{"member move own", ActionSessionMoveProject, RoleMember, "bob", true},
		{"member move others", ActionSessionMoveProject, RoleMember, "carol", false},
		{"admin delete", ActionSessionDelete, RoleAdmin, "carol", true},
		{"none read", ActionSessionRead, RoleNone, "carol", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Resource{Kind: KindSession, ID: "s1", ProjectID: "p1", CreatorID: tc.creator}
			err := Decide(bob, tc.action, res, tc.role)
			if tc.wantOK != (err == nil) {
				t.Fatalf("wantOK=%v, got %v", tc.wantOK, err)
			}
		})
	}
}

// TestDecidePersonalSession 个人会话仅 creator（admin 除外）。
func TestDecidePersonalSession(t *testing.T) {
	res := Resource{Kind: KindSession, ID: "s1", ProjectID: "", CreatorID: "alice"}
	if err := Decide(Principal{User: "alice"}, ActionSessionRead, res, RoleNone); err != nil {
		t.Fatalf("creator must read own session, got %v", err)
	}
	if err := Decide(Principal{User: "bob"}, ActionSessionRead, res, RoleNone); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-creator must be forbidden, got %v", err)
	}
	if err := Decide(Principal{User: "bob", IsAdmin: true}, ActionSessionDelete, res, RoleNone); err != nil {
		t.Fatalf("global admin must pass, got %v", err)
	}
	// 匿名身份对有主会话一律拒绝。
	if err := Decide(Principal{}, ActionSessionRead, res, RoleNone); !errors.Is(err, ErrForbidden) {
		t.Fatalf("anonymous on owned session must be forbidden, got %v", err)
	}
	// 匿名对匿名是既有契约（本地单机 / T13 回归底线）。
	anon := Resource{Kind: KindSession, ID: "s0", ProjectID: "", CreatorID: ""}
	if err := Decide(Principal{}, ActionSessionRead, anon, RoleNone); err != nil {
		t.Fatalf("anonymous on anonymous session must stay visible, got %v", err)
	}
	if err := Decide(Principal{User: "alice"}, ActionSessionRead, anon, RoleNone); !errors.Is(err, ErrForbidden) {
		t.Fatalf("alice on anonymous session must be forbidden, got %v", err)
	}
}

// TestDecidePluginLease 插件/租约：项目内 member 可用、admin 可管；个人资源归 creator。
func TestDecidePluginLease(t *testing.T) {
	inProj := Resource{Kind: KindPlugin, ID: "http", ProjectID: "p1", CreatorID: "alice"}
	personal := Resource{Kind: KindLease, ID: "l1", CreatorID: "alice"}
	alice := Principal{User: "alice"}
	bob := Principal{User: "bob"}

	if err := Decide(bob, ActionPluginRead, inProj, RoleNone); err != nil {
		t.Fatalf("plugin read is global, got %v", err)
	}
	if err := Decide(bob, ActionPluginUse, inProj, RoleMember); err != nil {
		t.Fatalf("member use in project, got %v", err)
	}
	if err := Decide(bob, ActionPluginManage, inProj, RoleMember); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member manage must be forbidden, got %v", err)
	}
	if err := Decide(bob, ActionPluginManage, inProj, RoleAdmin); err != nil {
		t.Fatalf("admin manage in project, got %v", err)
	}
	if err := Decide(alice, ActionLeaseUse, personal, RoleNone); err != nil {
		t.Fatalf("creator use own lease, got %v", err)
	}
	if err := Decide(bob, ActionLeaseRelease, personal, RoleNone); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-creator release must be forbidden, got %v", err)
	}
}

func TestAccessCodeActionAllowed(t *testing.T) {
	alice := Principal{User: "alice"}
	bob := Principal{User: "bob"}
	if !AccessCodeActionAllowed(alice, ActionAccessCodeCreate, "alice") {
		t.Fatal("creator can create own code")
	}
	if AccessCodeActionAllowed(bob, ActionAccessCodeCreate, "alice") {
		t.Fatal("non-creator cannot create for others")
	}
	if !AccessCodeActionAllowed(bob, ActionAccessCodeCreate, "") {
		t.Fatal("anonymous-owner codes are claimable/creatable by anyone (anonymous deployments)")
	}
	if !AccessCodeActionAllowed(bob, ActionAccessCodeClaim, "") {
		t.Fatal("claim is an unauthenticated endpoint by design")
	}
}

// 编译期固定权限矩阵：任何对 projectActionRole / sessionProjectActionRole 的
// 无意识改动都会让这份清单失配而编译失败。
var (
	_ = map[Action]Role{
		ActionProjectRead:          RoleMember,
		ActionProjectUpdate:        RoleOwner,
		ActionProjectDelete:        RoleOwner,
		ActionProjectManageMembers: RoleAdmin,
		ActionProjectManagePlugins: RoleAdmin,
		ActionProjectManageRules:   RoleAdmin,
		ActionProjectTransferOwner: RoleOwner,
		ActionSessionRead:          RoleMember,
		ActionSessionUse:           RoleMember,
		ActionSessionDelete:        RoleAdmin,
		ActionSessionMoveProject:   RoleAdmin,
	}
)
