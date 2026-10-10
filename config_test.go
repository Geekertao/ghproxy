package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// 配置重载与请求读取并发时不应发生数据竞争，且读取方始终看到完整一致的快照。
func TestConfigReloadConcurrentWithRead(t *testing.T) {
	// 保存并恢复所有被修改的全局状态，避免污染其他测试。
	origConfig := config.Load()
	oldOut := log.Out
	oldLevel := log.GetLevel()
	log.SetOutput(io.Discard)
	t.Cleanup(func() {
		config.Store(origConfig)
		log.SetOutput(oldOut)
		log.SetLevel(oldLevel)
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	write := func(n int) error {
		data := fmt.Sprintf(
			`{"debug":false,"whiteList":["v%d"],"requestLimit":{"limitRate":%d}}`,
			n, n,
		)
		return os.WriteFile(path, []byte(data), 0o644)
	}

	if err := write(0); err != nil {
		t.Fatal(err)
	}
	if err := loadConfigFrom(path); err != nil {
		t.Fatalf("初始加载失败: %v", err)
	}

	const rounds = 200
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 写方：持续重载配置。
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for n := 1; n <= rounds; n++ {
			if err := write(n); err != nil {
				t.Errorf("写入配置失败: %v", err)
				return
			}
			if err := loadConfigFrom(path); err != nil {
				t.Errorf("重载配置失败: %v", err)
				return
			}
		}
	}()

	// 读方：持续读取，验证同一快照内字段一致（whiteList 与 limitRate 对应）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				cfg := config.Load()
				if cfg == nil {
					t.Error("config 不应为 nil")
					return
				}
				if len(cfg.WhiteList) == 1 {
					want := cfg.WhiteList[0]
					got := fmt.Sprintf("v%d", cfg.RequestLimit.LimitRate)
					if want != got {
						t.Errorf("快照不一致: whiteList=%q, limitRate=%d", want, cfg.RequestLimit.LimitRate)
						return
					}
				}
			}
		}
	}()

	wg.Wait()

	// 最终配置应等于最后一次写入。
	got := config.Load()
	if got.RequestLimit.LimitRate != rounds {
		t.Fatalf("最终 limitRate 应为 %d，实际 %d", rounds, got.RequestLimit.LimitRate)
	}
}

// 加载不存在的文件应返回错误且不破坏已有配置。
func TestLoadConfigFromMissingFileKeepsOldConfig(t *testing.T) {
	before := config.Load()
	if err := loadConfigFrom(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("加载不存在的文件应返回错误")
	}
	if config.Load() != before {
		t.Fatal("加载失败时不应替换已有配置")
	}
}
