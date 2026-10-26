package sign

import (
	"context"
	"testing"
)

// 样本来自浏览器里真实调用同一份 wasm 的输出（2026-10-26 实测），
// 用来锁死 Go 侧调用约定：参数顺序、内存对齐、返回指针读取方式。
func TestSignMatchesBrowser(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close(ctx)

	cases := []struct{ ts, uuid, want string }{
		{"1793000000000", "abcdef0123456789abcdef0123456789", "1:31cdec9a2508e9f2af304567114e27e85097a85c3d1b92097e03f1e784fa76df"},
		{"1", "2", "1:6e445c737e5e5dfd96604a608964c02cbc03a34549175bcab005de9033121967"},
	}
	for _, c := range cases {
		got, err := s.Sign(ctx, c.ts, c.uuid)
		if err != nil {
			t.Fatalf("Sign(%q,%q): %v", c.ts, c.uuid, err)
		}
		if got != c.want {
			t.Errorf("Sign(%q,%q) = %q, want %q", c.ts, c.uuid, got, c.want)
		}
	}
}

func BenchmarkSign(b *testing.B) {
	ctx := context.Background()
	s, err := New(ctx)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer s.Close(ctx)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Sign(ctx, "1793000000000", "abcdef0123456789abcdef0123456789"); err != nil {
			b.Fatal(err)
		}
	}
}
