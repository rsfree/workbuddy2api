package upstream

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
)

func BenchmarkEgressClientLookup(b *testing.B) {
	c := New()
	c.SetEgress(egress.New(egress.Config{
		Mode:  egress.ModeLocal,
		Local: egress.Local{IPs: []string{"10.1.0.1", "10.1.0.2", "10.1.0.3", "10.1.0.4"}},
	}))
	accounts := make([]*auth.Auth, 16)
	for i := range accounts {
		accounts[i] = &auth.Auth{UID: string(rune('a' + i))}
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.clientPairFor(accounts[i%len(accounts)])
			i++
		}
	})
}
