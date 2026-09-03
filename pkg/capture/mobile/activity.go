package mobile

import (
	"sync/atomic"
)

// Activity 是移动代理抓包源的运行时活动追踪器（可选注入）。
//
// pipeline 把它注入 MobileConfig，使 租约快照能向控制面暴露
// 「手机是否已连接 / 数据是否在流动」的精准状态——source 实例是
// capture_task run 的局部变量，控制面无法直接触达，追踪器作为共享
// 探针跨越此边界。所有字段原子更新，Snapshot 无锁。
type Activity struct {
	activeConns  atomic.Int64  // 当前开放连接数（open +1 / close -1）
	totalConns   atomic.Uint64 // 累计打开连接数
	lastDataUnix atomic.Int64  // 最近一次收到数据的 unix 毫秒（0=从未）
	totalBytes   atomic.Uint64 // 累计接收应用层字节
}

// NewActivity 创建零值活动追踪器。
func NewActivity() *Activity { return &Activity{} }

// ActivitySnapshot 是追踪器的无锁读快照（与 proto ProxyLeaseState 的
// 活动字段一一对应）。
type ActivitySnapshot struct {
	ActiveConns  int64
	TotalConns   uint64
	LastDataUnix int64
	TotalBytes   uint64
}

// Snapshot 返回当前活动快照；nil 接收者返回零值。
func (a *Activity) Snapshot() ActivitySnapshot {
	if a == nil {
		return ActivitySnapshot{}
	}
	return ActivitySnapshot{
		ActiveConns:  a.activeConns.Load(),
		TotalConns:   a.totalConns.Load(),
		LastDataUnix: a.lastDataUnix.Load(),
		TotalBytes:   a.totalBytes.Load(),
	}
}
