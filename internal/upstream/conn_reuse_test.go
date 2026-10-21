package upstream

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdleBodyCloseKeepsConnectionReusable(t *testing.T) {
	var newConns int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt32(&newConns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	tr := &http.Transport{}
	client := &http.Client{Transport: tr}

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("req %d: %v", i, err)
		}
		body := monitorBody(resp.Body, 500*time.Millisecond, cancel)
		if _, err := io.Copy(io.Discard, body); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if err := body.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&newConns); got != 1 {
		t.Fatalf("3 次请求建了 %d 条 TCP 连接（期望复用为 1）", got)
	}
}
