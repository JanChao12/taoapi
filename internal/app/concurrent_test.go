package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy.local/workbuddy-api/internal/pool"
	"workbuddy.local/workbuddy-api/internal/provider"
)

// TestAccountsConcurrentActions 并发压测面板操作与调度的共享状态。
//
// -race 在本机不可用（无 gcc），用高强度并发代替：
// 同时发起 enable/disable/refresh/checkin/列表读 与 TryChat 调度，
// 任何 map 并发读写都会让测试进程直接 panic。
func TestAccountsConcurrentActions(t *testing.T) {
	deps, store, srv := newServeTestEnv(t, "sse-deepseek-noeffort.txt", "uid-a", "uid-b")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 并发面板操作
	actions := []string{"enable", "disable", "refresh", "checkin"}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			uid := "uid-a"
			if n%2 == 1 {
				uid = "uid-b"
			}
			for j := 0; j < 30; j++ {
				select {
				case <-stop:
					return
				default:
				}
				act := actions[j%len(actions)]
				body := fmt.Sprintf(`{"uid":%q,"action":%q}`, uid, act)
				resp, err := http.Post(srv.URL+"/api/accounts/action",
					"application/json", strings.NewReader(body))
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}(i)
	}

	// 并发调度（TryChat 内部会写账号状态）
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := provider.ChatRequest{Model: "workbuddy/deepseek-v4.1-flash"}
		for j := 0; j < 30; j++ {
			select {
			case <-stop:
				return
			default:
			}
			sess, err := TryChat(context.Background(), deps, deps.Chatter, req)
			if err == nil && sess != nil {
				sess.Close()
			}
		}
	}()

	// 并发读列表与 pool 快照
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 60; j++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = store.PoolAccounts(time.Now())
			resp, err := http.Get(srv.URL + "/api/accounts")
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
	}()

	// 等 3 秒后收工
	go func() {
		time.Sleep(3 * time.Second)
		close(stop)
	}()
	wg.Wait()

	// 收尾校验：两个账号都还在、状态合法
	for _, uid := range []string{"uid-a", "uid-b"} {
		a, ok := store.Get(uid)
		if !ok {
			t.Fatalf("并发后账号 %s 丢失", uid)
		}
		if a.Status != "" && !pool.Status(a.Status).Schedulable() &&
			a.Status != pool.StatusBanned && a.Status != pool.StatusAuthExpired {
			t.Errorf("账号 %s 状态异常: %q", uid, a.Status)
		}
	}
	_ = deps
}
