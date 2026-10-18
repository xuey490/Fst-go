package transfer

import (
	"context"
	"sync"
	"time"
)

// limiter 是一个按字节计量的令牌桶，用于传输限速（发送端与接收端共用同一个实例）。
//
// 说明：不引入 golang.org/x/time/rate，实现只需「等待 n 字节被允许」这一件事；
// 令牌按单调时钟补充，burst 取 256KB（一个写块），避免小包被拆成过多 sleep。
type limiter struct {
	mu     sync.Mutex
	rate   float64 // 每秒允许的字节数
	burst  float64 // 桶容量（单次最大突发）
	tokens float64
	last   time.Time
}

// newLimiter 按每秒字节数创建限速器。
//
// 参数:
//   - bytesPerSec: int64 — 每秒允许的字节数；<=0 返回 nil（表示不限速）
//
// 返回:
//   - *limiter — 限速器；nil 时调用方应跳过等待
func newLimiter(bytesPerSec int64) *limiter {
	if bytesPerSec <= 0 {
		return nil
	}
	burst := float64(bytesPerSec)
	if burst > 256<<10 {
		burst = 256 << 10
	}
	if burst < 4<<10 {
		burst = 4 << 10
	}
	return &limiter{rate: float64(bytesPerSec), burst: burst, tokens: burst, last: time.Now()}
}

// wait 阻塞到可以发送 n 字节为止，并扣减对应令牌。
//
// 参数:
//   - ctx: context.Context — 取消/截止；传输被取消时尽快返回
//   - n: int — 本轮要发送的字节数
//
// 返回:
//   - error — ctx 结束时返回其错误；正常等待返回 nil
//
// 并发：内部持锁补充令牌，多个文件共享同一限速器时总速率仍然成立。
func (l *limiter) wait(ctx context.Context, n int) error {
	if l == nil || n <= 0 {
		return nil
	}
	need := float64(n)
	// 超过桶容量的大块先拆成 burst 大小，避免永远等不到足够令牌
	for need > l.burst {
		if err := l.wait(ctx, int(l.burst)); err != nil {
			return err
		}
		need -= l.burst
	}
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
		if l.tokens >= need {
			l.tokens -= need
			l.mu.Unlock()
			return nil
		}
		deficit := need - l.tokens
		l.mu.Unlock()
		delay := time.Duration(deficit / l.rate * float64(time.Second))
		if delay < 5*time.Millisecond {
			delay = 5 * time.Millisecond
		}
		if delay > 200*time.Millisecond {
			delay = 200 * time.Millisecond // 分片等待，保证取消能及时生效
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}
