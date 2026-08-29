package main

import "time"

// backoff 实现与 SDK RunRegisterLoop 相同的指数退避策略：首次 1s，
// 每次翻倍，上限 30s；成功后 Reset 归位。
type backoff struct {
	cur time.Duration
	max time.Duration
}

func newBackoff() *backoff {
	return &backoff{cur: time.Second, max: 30 * time.Second}
}

// Next 返回当前退避时长并翻倍（封顶 max）。
func (b *backoff) Next() time.Duration {
	d := b.cur
	b.cur = b.cur * 2
	if b.cur > b.max {
		b.cur = b.max
	}
	return d
}

// Reset 将退避归位到首次时长（连续成功后调用）。
func (b *backoff) Reset() {
	b.cur = time.Second
}
