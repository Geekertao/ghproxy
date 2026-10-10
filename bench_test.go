package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

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

func benchNew(b *testing.B, ipCount int, limit int64) {
	l := newRateLimiter()
	l.sweepEvery = time.Hour // 排除回收开销，只衡量单请求处理成本
	ips := makeIPs(ipCount)
	now := time.Now()
	// 预填满各 IP 的窗口，使热路径处于「已满」状态。
	for _, ip := range ips {
		for j := int64(0); j < limit; j++ {
			l.allow(ip, limit, now)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			l.allow(ips[i%ipCount], limit, now)
			i++
		}
	})
}

func benchOld(b *testing.B, ipCount int, limit int64) {
	entries := make(map[string][]time.Time, ipCount)
	var mu sync.Mutex
	ips := makeIPs(ipCount)
	now := time.Now()
	for _, ip := range ips {
		for j := int64(0); j < limit; j++ {
			entries[ip] = append(entries[ip], now)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			allowOld(entries, &mu, ips[i%ipCount], limit, now)
			i++
		}
	})
}

func BenchmarkLimiterNew_SingleIP(b *testing.B) { benchNew(b, 1, 100) }
func BenchmarkLimiterNew_ManyIPs(b *testing.B)  { benchNew(b, 1000, 100) }
func BenchmarkLimiterOld_SingleIP(b *testing.B) { benchOld(b, 1, 100) }
func BenchmarkLimiterOld_ManyIPs(b *testing.B)  { benchOld(b, 1000, 100) }

// BenchmarkLimiterSweep 衡量一次回收（清理大量过期 IP）的开销。
func BenchmarkLimiterSweep(b *testing.B) {
	const n = 5000
	ips := makeIPs(n)
	base := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := newRateLimiter()
		l.sweepEvery = time.Hour
		for _, ip := range ips {
			l.allow(ip, 10, base)
		}
		l.sweepEvery = 0
		l.allow("192.168.0.1", 10, base.Add(2*time.Minute))
	}
}
