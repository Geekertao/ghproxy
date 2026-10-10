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
		if !l.allowAt("1.1.1.1", 0, now) {
			t.Fatalf("关闭限流时应始终放行，第 %d 次被拒绝", i+1)
		}
		if !l.allowAt("2.2.2.2", -5, now) {
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
		if !l.allowAt("1.1.1.1", limit, now) {
			t.Fatalf("第 %d 次请求应放行（恰好达到限额）", i+1)
		}
	}
}

// 3. 超过限额时应被拒绝。
func TestLimiterOverLimitRejected(t *testing.T) {
	l := newTestLimiter()
	now := time.Now()
	const limit = 2
	if !l.allowAt("1.1.1.1", limit, now) || !l.allowAt("1.1.1.1", limit, now) {
		t.Fatal("前两次请求应放行")
	}
	if l.allowAt("1.1.1.1", limit, now) {
		t.Fatal("第 3 次请求应被拒绝")
	}
	if l.allowAt("1.1.1.1", limit, now) {
		t.Fatal("超限后应持续拒绝")
	}
}

// 4. 时间窗口过期后应恢复放行，并验证窗口边界不出现 off-by-one。
func TestLimiterWindowExpiry(t *testing.T) {
	l := newTestLimiter()
	t0 := time.Now()
	const limit = 2
	l.allowAt("1.1.1.1", limit, t0)
	l.allowAt("1.1.1.1", limit, t0)
	if l.allowAt("1.1.1.1", limit, t0.Add(30*time.Second)) {
		t.Fatal("窗口内应继续拒绝")
	}
	// 恰好在窗口边界（age == window）时记录仍有效，应拒绝。
	if l.allowAt("1.1.1.1", limit, t0.Add(time.Minute)) {
		t.Fatal("窗口边界上的记录仍应计入，必须拒绝")
	}
	// 刚超过一个窗口后，旧记录过期，应恢复放行。
	if !l.allowAt("1.1.1.1", limit, t0.Add(time.Minute+time.Nanosecond)) {
		t.Fatal("窗口过期后应恢复放行")
	}
}

// 5. 多个 IP 之间互不影响。
func TestLimiterMultipleIPsIndependent(t *testing.T) {
	l := newTestLimiter()
	now := time.Now()
	const limit = 1
	if !l.allowAt("a", limit, now) {
		t.Fatal("a 第一次应放行")
	}
	if l.allowAt("a", limit, now) {
		t.Fatal("a 第二次应拒绝")
	}
	if !l.allowAt("b", limit, now) {
		t.Fatal("b 不应受 a 的限制影响")
	}
	if l.allowAt("b", limit, now) {
		t.Fatal("b 第二次应拒绝")
	}
}

// 6a. 同一 IP 的并发请求（固定时间注入）：放行总数必须恰好等于限额。
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
			if l.allowAt("same", limit, now) {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()

	if allowed != limit {
		t.Fatalf("并发放行数应为 %d，实际为 %d", limit, allowed)
	}
}

// 6b. 使用真实生产入口（锁内采样 time.Now）并发请求：
// 放行数恰当，且 entries[ip] 必须保持按时间升序，验证时间戳顺序修复。
func TestLimiterConcurrentRealClockOrdering(t *testing.T) {
	l := newTestLimiter()
	const limit = 100
	const goroutines = 2000

	var allowed int64
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.allow("same", limit) {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()

	if allowed != limit {
		t.Fatalf("并发放行数应为 %d，实际为 %d", limit, allowed)
	}
	times := l.entries["same"]
	if len(times) != limit {
		t.Fatalf("窗口内记录数应为 %d，实际 %d", limit, len(times))
	}
	for i := 1; i < len(times); i++ {
		if times[i].Before(times[i-1]) {
			t.Fatalf("时间戳乱序：索引 %d (%v) 早于 %d (%v)", i, times[i], i-1, times[i-1])
		}
	}
}

// 7. 大量不同 IP 请求后，过期条目应被回收，避免无界增长。
func TestLimiterReclaimsInactiveIPs(t *testing.T) {
	l := newTestLimiter()
	base := time.Now()
	const n = 5000
	for i := 0; i < n; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		l.allowAt(ip, 10, base)
	}
	if got := len(l.entries); got != n {
		t.Fatalf("应记录 %d 个 IP，实际 %d", n, got)
	}

	// 强制在窗口之后触发一次回收，清掉全部不活跃 IP。
	l.sweepEvery = 0
	after := base.Add(2 * time.Minute)
	if !l.allowAt("192.168.0.1", 10, after) {
		t.Fatal("新 IP 应放行")
	}
	if got := len(l.entries); got != 1 {
		t.Fatalf("过期 IP 应被回收，仅剩新 IP（1 个），实际 %d", got)
	}
}

// 8. 直接验证 sweep 的目标代码路径：裁剪活跃 IP 的过期记录、回收不活跃 IP，
// 且回收不破坏仍然有效的窗口内记录。
func TestLimiterSweepPrunesActiveAndReclaimsInactive(t *testing.T) {
	l := newTestLimiter()
	base := time.Now()

	// 直接构造 entries，使旧记录在 sweep 被调用时确实仍然存在，
	// 不经过 allowAt 触发的 prune，从而真正覆盖 sweepLocked 的裁剪逻辑。
	l.entries["active"] = []time.Time{base, base.Add(80 * time.Second)}
	l.entries["stale"] = []time.Time{base}

	now := base.Add(90 * time.Second) // cutoff = now - window = base + 30s
	l.mu.Lock()
	l.sweepLocked(now)
	l.mu.Unlock()

	// 活跃 IP：旧记录被裁剪，窗口内记录保留。
	active := l.entries["active"]
	if len(active) != 1 || !active[0].Equal(base.Add(80*time.Second)) {
		t.Fatalf("active 应仅保留窗口内的 1 条记录，实际 %v", active)
	}
	// 不活跃 IP：被正确回收。
	if _, ok := l.entries["stale"]; ok {
		t.Fatal("不活跃 IP 应被回收")
	}
	// 回收不破坏仍有效的限流：active 在窗口内已有 1 条记录，limit=1 时下一次应被拒绝。
	if l.allowAt("active", 1, now) {
		t.Fatal("活跃 IP 的窗口内记录应仍然生效（limit=1 时应拒绝）")
	}
}
