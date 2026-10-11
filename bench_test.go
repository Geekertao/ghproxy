package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ===========================================================================
// 基准口径说明（改代码时请同步本段，并由 TestBenchmarkPrimeStates 守护）
//
// 【为什么不能宣称「两边记录数相同」】
// 两个实现的「饱和态」（此后每次调用都被拒绝的状态）记录条数天然不同：
//   旧：拒绝条件 len(entries[ip]) >  limit  → 饱和记录数 = limit+1
//   新：拒绝条件 len(times)       >= limit  → 饱和记录数 = limit
// 在 limit=100 时即 101 条 vs 100 条。这 1 条之差**正是本 PR 要修正的边界偏差**
// （旧实现窗口内会多放行一次），不是可以「统一」掉的实现细节：
//   - 新实现**无法**经公开入口（allow / allowAt）达到 limit+1 条记录：第 limit+1 次
//     调用已被拒绝，不会追加。若要用直接改 map 的方式硬凑 limit+1，那是运行期
//     不可达的状态，不能代表新实现。
//   - 旧实现若只预填 limit 条，则第 1 次调用会先放行并追加，计时区间内混入一次
//     与其余调用路径不同的样本。
// （此前版本的本文件曾把新实现写成「预填 limit+1 次 ⇒ 保留 limit+1 条」，并把
//  两边记录数写成「一致（101）」——经实测为 100 条，注释与代码不符，已更正。）
//
// 【本文件采用的做法】
// 不做「记录数相同」的伪统一，而是让**每一侧各自准备在自己的饱和拒绝态**：
// 计时区间内每次调用都是同一条「已饱和 → 拒绝」路径，不含放行与追加；
// 两侧各自的记录条数由 primeNewRejecting / primeOldRejecting 返回，
// 并由 TestBenchmarkPrimeStates 断言，避免注释再次与代码脱节。
//
// 【计时口径】
//   - BenchmarkLimiterNew_* / BenchmarkLimiterOld_*：注入固定时间 → 纯算法成本，两者可互比
//   - BenchmarkLimiterNewRealClock_*：走生产入口 allow()（锁内采样 time.Now）→ 含真实
//     时钟成本，只能与 NewRealClock 系列比，不可与前两者混比
//
// 【这不是放行路径的对比】
// 同一记录序列下新旧放行次数不同（limit+1 vs limit），不存在可直接对比的同一
// 放行状态；如需放行路径数据，必须单独说明状态与记录数，不能与本文件的数字混用。
// ===========================================================================

const (
	// benchLimit 是基准统一使用的限流额度（生产 config.json 为 500；此处取 100 以
	// 控制单次运行时长，两侧一致即可比）。
	benchLimit int64 = 100
	// benchIPsMany 是「多 IP」场景的 IP 数量。
	benchIPsMany = 1000
)

// workerStride 是并行基准中相邻 worker 的起始 IP 偏移步长。
// 每个 worker 都从 i := 0 开始会让它们初始全部争抢同一个 IP，使多 IP 场景实际
// 退化成单 IP；给每个 worker 一个互不相同的起始偏移后，访问分布才真正覆盖多个 IP。
// 取一个与常见 IP 数量互质的较大质数，使起始位置在 IP 空间上分散。
const workerStride = 1000003

// parallelStart 返回当前 worker 的起始索引，仅在每个 worker 启动时调用一次，
// 不进入 pb.Next() 循环，因此不引入每次请求的额外开销。
func parallelStart(seq *atomic.Uint64, ipCount int) int {
	return int(seq.Add(1)-1) * workerStride % ipCount
}

// allowOld 复刻优化前的限流实现，仅用于基准对比：
// 每个请求都加全局锁并遍历所有 IP，且从不删除过期 IP 键。
func allowOld(entries map[string][]time.Time, mu *sync.Mutex, ip string, limit int64, now time.Time) bool {
	mu.Lock()
	for k, times := range entries {
		var recent []time.Time
		for _, ts := range times {
			if now.Sub(ts) <= time.Minute {
				recent = append(recent, ts)
			}
		}
		entries[k] = recent
	}
	if limit > 0 && len(entries[ip]) > int(limit) {
		mu.Unlock()
		return false
	}
	entries[ip] = append(entries[ip], now)
	mu.Unlock()
	return true
}

func makeIPs(n int) []string {
	ips := make([]string, n)
	for i := range ips {
		ips[i] = fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
	}
	return ips
}

// expectedNewRejectingRecords 返回新实现饱和拒绝态的每 IP 记录条数。
func expectedNewRejectingRecords(limit int64) int { return int(limit) }

// expectedOldRejectingRecords 返回旧实现饱和拒绝态的每 IP 记录条数。
func expectedOldRejectingRecords(limit int64) int { return int(limit + 1) }

// primeNewRejecting 把新实现预置到饱和拒绝态：每个 IP 经公开入口放行 limit 次
// （第 limit 次之后 len == limit，后续调用走拒绝路径）。返回实测记录条数。
// 预置期间 l.sweepEvery 应已设为不会触发回收的值。
func primeNewRejecting(l *rateLimiter, ips []string, limit int64, now time.Time) int {
	for _, ip := range ips {
		for j := int64(0); j < limit; j++ {
			if !l.allowAt(ip, limit, now) {
				panic("基准预置失败：limit 次以内不应被拒绝")
			}
		}
	}
	return len(l.entries[ips[0]])
}

// primeNewRejectingRealClock 同 primeNewRejecting，但走生产入口 allow()（真实时钟）。
func primeNewRejectingRealClock(l *rateLimiter, ips []string, limit int64) int {
	for _, ip := range ips {
		for j := int64(0); j < limit; j++ {
			if !l.allow(ip, limit) {
				panic("基准预置失败：limit 次以内不应被拒绝")
			}
		}
	}
	return len(l.entries[ips[0]])
}

// primeOldRejecting 把旧实现预置到饱和拒绝态：每个 IP 直接写入 limit+1 条记录
// （旧实现拒绝条件为 len > limit，故饱和态为 limit+1 条）。返回实测记录条数。
func primeOldRejecting(entries map[string][]time.Time, ips []string, limit int64, now time.Time) int {
	for _, ip := range ips {
		for j := int64(0); j < limit+1; j++ {
			entries[ip] = append(entries[ip], now)
		}
	}
	return len(entries[ips[0]])
}

func benchNew(b *testing.B, ipCount int, limit int64) {
	l := newRateLimiter()
	l.sweepEvery = time.Hour // 排除回收开销，只衡量单请求处理成本
	ips := makeIPs(ipCount)
	now := time.Now()

	b.ReportAllocs()
	b.StopTimer()
	if got, want := primeNewRejecting(l, ips, limit, now), expectedNewRejectingRecords(limit); got != want {
		b.Fatalf("新实现饱和态记录数 = %d，期望 %d", got, want)
	}
	// 注意：ResetTimer 只归零累计耗时，**不会启动**计时器；StopTimer 之后必须再
	// 显式 StartTimer，否则计时区间恒为 0，框架会把 b.N 一路放大到上限后才停。
	b.ResetTimer()
	b.StartTimer()

	var seq atomic.Uint64
	b.RunParallel(func(pb *testing.PB) {
		i := parallelStart(&seq, ipCount)
		for pb.Next() {
			l.allowAt(ips[i%ipCount], limit, now)
			i++
		}
	})
}

// benchNewRealClock 衡量生产入口（锁内采样 time.Now）的单请求成本。
// 时钟口径与 New/Old 两组不同，结果只能在这两者之间比较。
// 预置与循环都用真实时钟：只要单次 benchmark 的循环耗时远小于 window（1 分钟），
// 记录不会被裁剪，循环内始终保持「已饱和 → 拒绝」。
func benchNewRealClock(b *testing.B, ipCount int, limit int64) {
	l := newRateLimiter()
	l.sweepEvery = time.Hour
	ips := makeIPs(ipCount)

	b.ReportAllocs()
	b.StopTimer()
	if got, want := primeNewRejectingRealClock(l, ips, limit), expectedNewRejectingRecords(limit); got != want {
		b.Fatalf("新实现（真实时钟）饱和态记录数 = %d，期望 %d", got, want)
	}
	b.ResetTimer()
	b.StartTimer()

	var seq atomic.Uint64
	b.RunParallel(func(pb *testing.PB) {
		i := parallelStart(&seq, ipCount)
		for pb.Next() {
			l.allow(ips[i%ipCount], limit)
			i++
		}
	})
}

func benchOld(b *testing.B, ipCount int, limit int64) {
	entries := make(map[string][]time.Time, ipCount)
	var mu sync.Mutex
	ips := makeIPs(ipCount)
	now := time.Now()

	b.ReportAllocs()
	b.StopTimer()
	if got, want := primeOldRejecting(entries, ips, limit, now), expectedOldRejectingRecords(limit); got != want {
		b.Fatalf("旧实现饱和态记录数 = %d，期望 %d", got, want)
	}
	b.ResetTimer()
	b.StartTimer()

	var seq atomic.Uint64
	b.RunParallel(func(pb *testing.PB) {
		i := parallelStart(&seq, ipCount)
		for pb.Next() {
			allowOld(entries, &mu, ips[i%ipCount], limit, now)
			i++
		}
	})
}

func BenchmarkLimiterNew_SingleIP(b *testing.B) { benchNew(b, 1, benchLimit) }
func BenchmarkLimiterNew_ManyIPs(b *testing.B)  { benchNew(b, benchIPsMany, benchLimit) }
func BenchmarkLimiterOld_SingleIP(b *testing.B) { benchOld(b, 1, benchLimit) }
func BenchmarkLimiterOld_ManyIPs(b *testing.B)  { benchOld(b, benchIPsMany, benchLimit) }

func BenchmarkLimiterNewRealClock_SingleIP(b *testing.B) { benchNewRealClock(b, 1, benchLimit) }
func BenchmarkLimiterNewRealClock_ManyIPs(b *testing.B) {
	benchNewRealClock(b, benchIPsMany, benchLimit)
}

// sweepRecordsPerIP 是回收基准中每个 IP 的记录条数：经公开入口写入 10 条
// （sweepLimit 恰好为 10，故 10 次调用全部放行、全部被记录）。
const (
	sweepLimit        int64 = 10
	sweepRecordsPerIP       = 10
	sweepIPCount            = 5000
)

// fixSweepLimiter 构造一个「已填充 n 个 IP、每个 IP sweepRecordsPerIP 条记录、
// 且下一次 allow 必然触发回收的限流器。
// 填充走限流器正常入口（allowAt），每个 IP 调用 sweepLimit 次，因此每个 IP 恰好
// 记录 10 条（t == base），并在 [base+2min] 触发回收时全部过期（window=1min，
// cutoff = base+1min，10 条均早于 cutoff）。
//
// 返回值中 l.sweepEvery 已置 0，只要传入的触发时刻**不早于** l.lastSweep
// （= 本函数内部 newRateLimiter 时的 time.Now()），下一次 allow 就会触发 sweepLocked。
// 因此调用方必须把触发时刻算成本轮填充的时刻（base）再加偏移，不能拿到循环外
// 复用同一个绝对时刻：那会随着真实时间前进使触发时刻反而早于 l.lastSweep，
// 此时 now.Sub(lastSweep) 为负、sweepEvery=0 也满足不了条件，回收会**静默不触发**，
// 基准就变成了在测一条空的拒绝路径。
//
// 填充成本是否计入计时区间**由调用方决定**：
//   - BenchmarkLimiterSweep 在 StopTimer 状态下调用以排除填充成本；
//   - BenchmarkLimiterSweepWithFill 则在计时区间内调用，有意把填充计入。
func fixSweepLimiter(n int, base time.Time) *rateLimiter {
	ips := makeIPs(n)
	l := newRateLimiter()
	l.sweepEvery = time.Hour // 填充期间不触发回收
	for _, ip := range ips {
		for j := 0; j < sweepRecordsPerIP; j++ {
			if !l.allowAt(ip, sweepLimit, base) {
				panic("回收基准填充失败：sweepLimit 次以内不应被拒绝")
			}
		}
	}
	// 防注释与代码脱节：每个 IP 必须恰好 sweepRecordsPerIP 条记录。
	for _, ip := range ips {
		if got := len(l.entries[ip]); got != sweepRecordsPerIP {
			panic(fmt.Sprintf("回收基准填充记录数 = %d，期望 %d", got, sweepRecordsPerIP))
		}
	}
	l.sweepEvery = 0 // 下一次 allow 立即触发 sweepLocked
	return l
}

// BenchmarkLimiterSweep 只衡量**一次回收操作**本身。
//
// 工作负载：sweepIPCount(5000) 个 IP × sweepRecordsPerIP(10) 条记录 = 50,000 条，
// 全部已在窗口外过期 → 一次 sweepLocked 应把 5000 个 IP 键全部删除。
//
// **计时范围**：构造限流器与填充 5000 个 IP 属于初始化成本，通过
// StopTimer/StartTimer 排除在计时区间之外；报告中的 ns/op、B/op、allocs/op
// 只反映 sweepLocked 及其触发它的那一次 allow 调用本身，可解释为「单次 sweep
// 的成本」，**不含**填充成本。
//
// **触发时刻每轮重算**：base 与 next 都在循环内、相对于本轮填充时刻计算。
// 若把 base 提到循环外，长时间运行后 next 会早于新一轮限流器的 lastSweep
// （= newRateLimiter 时的真实 now），now.Sub(lastSweep) 为负 → 回收不触发。
func BenchmarkLimiterSweep(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		base := time.Now()
		l := fixSweepLimiter(sweepIPCount, base)
		next := base.Add(2 * time.Minute) // 相对本轮填充时刻，保证晚于 l.lastSweep
		b.StartTimer()

		l.allowAt("192.168.0.1", sweepLimit, next) // 触发 sweepLocked
	}
}

// BenchmarkLimiterSweepWithFill 衡量**完整周期**：新建限流器 + 填充 5000 个 IP
// （每个 10 条记录）+ 触发一次回收。
//
// **计时范围**：与 BenchmarkLimiterSweep 相反，它**有意把填充成本计入**被测区间，
// 因此其结果**不能**当作纯回收耗时解释；保留它是为了观察「填充 + 回收」整体开销
// 随规模的变化。
//
// 触发时刻同样每轮重算（见 BenchmarkLimiterSweep 的说明），避免长跑后回收不触发。
func BenchmarkLimiterSweepWithFill(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		base := time.Now()
		l := fixSweepLimiter(sweepIPCount, base)
		next := base.Add(2 * time.Minute)
		l.allowAt("192.168.0.1", sweepLimit, next)
	}
}

// TestBenchmarkPrimeStates 断言基准预置状态与文件头部注释所述一致：
// 新实现饱和态 = limit 条，旧实现饱和态 = limit+1 条，且饱和后调用被拒绝、
// 记录数不再增长。两者相差 1 是边界语义差异的必然结果，不可宣称「两边相同」。
func TestBenchmarkPrimeStates(t *testing.T) {
	const limit = int64(10)
	now := time.Now()
	ips := []string{"10.0.0.1"}

	l := newTestLimiter()
	if got, want := primeNewRejecting(l, ips, limit, now), expectedNewRejectingRecords(limit); got != want {
		t.Fatalf("新实现饱和态记录数 = %d，期望 %d", got, want)
	}
	if l.allowAt(ips[0], limit, now) {
		t.Fatal("新实现饱和态应拒绝")
	}
	if got, want := len(l.entries[ips[0]]), expectedNewRejectingRecords(limit); got != want {
		t.Fatalf("被拒绝后新实现记录数 = %d，期望保持不变 %d", got, want)
	}

	entries := map[string][]time.Time{}
	var mu sync.Mutex
	if got, want := primeOldRejecting(entries, ips, limit, now), expectedOldRejectingRecords(limit); got != want {
		t.Fatalf("旧实现饱和态记录数 = %d，期望 %d", got, want)
	}
	if allowOld(entries, &mu, ips[0], limit, now) {
		t.Fatal("旧实现饱和态应拒绝")
	}
	if got, want := len(entries[ips[0]]), expectedOldRejectingRecords(limit); got != want {
		t.Fatalf("被拒绝后旧实现记录数 = %d，期望保持不变 %d", got, want)
	}

	t.Logf("饱和态记录数：新实现 %d / 旧实现 %d（limit=%d，相差 1 即本 PR 修正的边界偏差）",
		expectedNewRejectingRecords(limit), expectedOldRejectingRecords(limit), limit)
}

// TestSweepFixtureRecordsAllExpired 断言回收基准的填充夹具与注释一致：
// 每个 IP 恰好 sweepRecordsPerIP 条记录，且在触发回收的时刻全部已过期
// （这才是「一次 sweep 删除 5000 个 IP 键」的前提）。
func TestSweepFixtureRecordsAllExpired(t *testing.T) {
	base := time.Now()
	l := fixSweepLimiter(64, base) // 用小规模验证夹具本身

	if got := len(l.entries); got != 64 {
		t.Fatalf("填充后 IP 数 = %d，期望 64", got)
	}
	for ip, times := range l.entries {
		if len(times) != sweepRecordsPerIP {
			t.Fatalf("IP %s 记录数 = %d，期望 %d", ip, len(times), sweepRecordsPerIP)
		}
	}

	next := base.Add(2 * time.Minute)
	cutoff := next.Add(-l.window)
	for ip, times := range l.entries {
		for i, ts := range times {
			if !ts.Before(cutoff) {
				t.Fatalf("IP %s 第 %d 条记录 %v 未早于 cutoff %v，回收时不会过期", ip, i, ts, cutoff)
			}
		}
	}

	if !l.allowAt("192.168.0.1", sweepLimit, next) {
		t.Fatal("新 IP 应放行")
	}
	if got := len(l.entries); got != 1 {
		t.Fatalf("回收后应只剩新 IP（1 个），实际 %d", got)
	}
	t.Logf("回收夹具：64 个 IP × %d 条记录全部过期，一次 sweep 后仅剩新 IP", sweepRecordsPerIP)
}

// TestSweepTriggersWithStaleBase 守护触发时刻的算法：base 取一个**远早于现在**的
// 时刻（模拟基准长跑很久之后，循环外的固定 base 已经过期），触发时刻相对 base
// 计算就会早于新一轮限流器的 lastSweep（= newRateLimiter 时的真实 now），
// now.Sub(lastSweep) 为负 → 即便 sweepEvery=0 也**不会**触发回收。
// 基准必须按此每轮重算触发时刻；本测试把该陷阱固定下来。
func TestSweepTriggersWithStaleBase(t *testing.T) {
	staleBase := time.Now().Add(-10 * time.Minute)
	l := fixSweepLimiter(64, staleBase)

	staleNext := staleBase.Add(2 * time.Minute) // 已早于真实 now
	if !staleNext.Before(l.lastSweep) {
		t.Fatal("前置条件不成立：staleNext 应早于 l.lastSweep")
	}

	if !l.allowAt("192.168.0.1", sweepLimit, staleNext) {
		t.Fatal("新 IP 应放行")
	}
	if got := len(l.entries); got != 65 {
		t.Fatalf("陈旧触发时刻下回收不应发生（应保留 64 个旧 IP + 1 个新 IP = 65），实际 %d", got)
	}

	// 相对本轮填充时刻重算后，回收必须发生。
	l2 := fixSweepLimiter(64, staleBase)
	freshNext := time.Now().Add(2 * time.Minute)
	if !l2.allowAt("192.168.0.1", sweepLimit, freshNext) {
		t.Fatal("新 IP 应放行")
	}
	if got := len(l2.entries); got != 1 {
		t.Fatalf("相对当前时刻的触发应完成回收（仅剩 1 个新 IP），实际 %d", got)
	}
	t.Log("已确认：触发时刻必须每轮相对 newRateLimiter 的真实时刻重算，否则回收静默不触发")
}
