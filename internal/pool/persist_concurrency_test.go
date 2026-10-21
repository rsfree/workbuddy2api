package pool

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 落盘与选号并发安全：落盘已移到 p.mu 之外，必须确认
//  1) 不会与 pick 抢锁导致死锁；
//  2) 快照与写盘之间没有数据竞争（-race 下跑）。
func TestFlushConcurrentWithPick(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	for i := 0; i < 50; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%03d", i), AccessToken: "token"})
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 选号侧：持续读池。
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = p.Pick()
				}
			}
		}()
	}

	// 写盘侧：持续置脏并落盘。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			p.mu.Lock()
			p.dirty.Store(true)
			p.mu.Unlock()
			p.Flush()
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// 并发首次见到同一出口时，只应保留一份 client（LoadOrStore 幂等）。
// 这里用 Flush 的并发写盘做同类验证：多个 goroutine 同时落盘不应互相覆盖。
func TestConcurrentFlushKeepsFileValid(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1", AccessToken: "token"})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				p.mu.Lock()
				p.dirty.Store(true)
				p.mu.Unlock()
				p.Flush()
			}
		}()
	}
	wg.Wait()

	p.mu.Lock()
	p.dirty.Store(true)
	p.mu.Unlock()
	p.Flush()

	// 落盘结果必须能被重新解析（并发写同一 tmp 会产出半截 JSON）。
	q := New(p.stateFp)
	if len(q.byUID) != 1 {
		t.Fatalf("落盘文件损坏或丢账号: %d", len(q.byUID))
	}
}
