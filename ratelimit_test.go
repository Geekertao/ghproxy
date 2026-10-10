package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestLimiter 关闭自动回收，便于测试精确控制时间。
func newTestLimiter() *rateLimiter {
	l := newRateLimiter()
	l.sweepEvery = time.Hour
	return l
}

// 1. 关闭限流（limit <= 0）时应始终放行且不记录任何状态。
func TestLimiterDisabled(t *testing.T) {
	l := newTestLimiter()
	now := time.Now()
	for i := 0; i < 1000; i++ {
		if !l.allow("1.1.1.1", 0, now) {
			t.Fatalf("关闭限流时应始终放行，第 %d 次被拒绝", i+1)
		}
		if !l.allow("2.2.2.2", -5, now) {
			t.Fatalf("limitRate 为负时应始终放行")
		}
	}
	if len(l.entries) != 0 {
		t.Fatalf("关闭限流时不应记录任何 IP，实际记录 %d 个", len(l.entries))
	}
}

// 2. 请求数恰好达到限额时应全部放行。
func TestLimiterExactlyAtLimit(t *testing.T) {
	l := newTestLimiter()
	now := time.Now()
	const limit = 3
	for i := 0; i < limit; i++ {
		if !l.allow("1.1.1.1", limit, now) {
			t.Fatalf("第 %d 次请求应放行（恰好达到限额）", i+1)
		}
	}
}

// 3. 超过限额时应被拒绝。
func TestLimiterOverLimitRejected(t *testing.T) {
	l := newTestLimiter()
	now := time.Now()
	const limit = 2
	if !l.allow("1.1.1.1", limit, now) || !l.allow("1.1.1.1", limit, now) {
		t.Fatal("前两次请求应放行")
	}
	if l.allow("1.1.1.1", limit, now) {
		t.Fatal("第 3 次请求应被拒绝")
	}
	if l.allow("1.1.1.1", limit, now) {
		t.Fatal("超限后应持续拒绝")
	}
}

// 4. 时间窗口过期后应恢复放行。
func TestLimiterWindowExpiry(t *testing.T) {
	l := newTestLimiter()
	t0 := time.Now()
	const limit = 2
	l.allow("1.1.1.1", limit, t0)
	l.allow("1.1.1.1", limit, t0)
	if l.allow("1.1.1.1", limit, t0.Add(30*time.Second)) {
		t.Fatal("窗口内应继续拒绝")
	}
	// 超过一个窗口后，旧的记录过期，应重新放行。
	if !l.allow("1.1.1.1", limit, t0.Add(time.Minute+time.Second)) {
		t.Fatal("窗口过期后应恢复放行")
	}
}

// 5. 多个 IP 之间互不影响。
func TestLimiterMultipleIPsIndependent(t *testing.T) {
	l := newTestLimiter()
	now := time.Now()
	const limit = 1
	if !l.allow("a", limit, now) {
		t.Fatal("a 第一次应放行")
	}
	if l.allow("a", limit, now) {
		t.Fatal("a 第二次应拒绝")
	}
	if !l.allow("b", limit, now) {
		t.Fatal("b 不应受 a 的限制影响")
	}
	if l.allow("b", limit, now) {
		t.Fatal("b 第二次应拒绝")
	}
}

// 6. 同一 IP 的并发请求：放行总数必须恰好等于限额。
func TestLimiterConcurrentSameIP(t *testing.T) {
	l := newTestLimiter()
	now := time.Now()
	const limit = 50
	const goroutines = 500

	var allowed int64
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.allow("same", limit, now) {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()

	if allowed != limit {
		t.Fatalf("并发放行数应为 %d，实际为 %d", limit, allowed)
	}
}

// 7. 大量不同 IP 请求后，过期条目应被回收，避免无界增长。
func TestLimiterReclaimsInactiveIPs(t *testing.T) {
	l := newRateLimiter() // 使用默认 window / sweepEvery
	base := time.Now()
	const n = 5000
	for i := 0; i < n; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		l.allow(ip, 10, base)
	}
	if got := len(l.entries); got != n {
		t.Fatalf("应记录 %d 个 IP，实际 %d", n, got)
	}

	// 一个窗口之后发起的请求会触发回收，清掉全部不活跃 IP。
	after := base.Add(2 * time.Minute)
	if !l.allow("192.168.0.1", 10, after) {
		t.Fatal("新 IP 应放行")
	}
	if got := len(l.entries); got != 1 {
		t.Fatalf("过期 IP 应被回收，仅剩新 IP（1 个），实际 %d", got)
	}
}

// 活跃 IP 的过期记录应在回收时被裁剪，但保留窗口内记录。
func TestLimiterSweepPrunesActiveIP(t *testing.T) {
	l := newTestLimiter()
	base := time.Now()
	// active 有一条旧记录和一条窗口内新记录。
	l.allow("active", 10, base)
	l.allow("active", 10, base.Add(80*time.Second))

	// 强制在 base+90s 触发一次回收：cutoff = base+30s，
	// active 的旧记录被裁剪，窗口内的新记录保留。
	l.sweepEvery = 0
	l.allow("other", 10, base.Add(90*time.Second))

	got := l.entries["active"]
	if len(got) != 1 {
		t.Fatalf("active 应保留 1 条窗口内记录，实际 %d 条", len(got))
	}
	if !got[0].Equal(base.Add(80 * time.Second)) {
		t.Fatalf("保留下来的应是窗口内的新记录")
	}
}
