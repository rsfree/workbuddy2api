package pool

import (
	"fmt"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func BenchmarkPickContended(b *testing.B) {
	p := New("")
	defer p.Close()
	for i := 0; i < 46; i++ {
		a := &auth.Auth{UID: fmt.Sprintf("uid-%03d", i), AccessToken: "t", Nickname: fmt.Sprintf("n%d", i)}
		p.Add(a)
		p.mu.Lock()
		e := p.byUID[a.UID]
		e.credits = int64(1000 + i*10)
		p.mu.Unlock()
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = p.Pick()
		}
	})
}
