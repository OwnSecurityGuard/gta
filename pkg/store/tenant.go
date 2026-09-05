package store

import "gta/pkg/authz"

// normalizeTenant 归一会话租户：空串（匿名/历史数据）统一落 authz.DefaultTenant，
// 保证租户 CAS（MoveSessionToProject）与查询过滤不会被 '' 与 'default' 的二义性击穿。
func normalizeTenant(t string) string {
	if t == "" {
		return authz.DefaultTenant
	}
	return t
}
