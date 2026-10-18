package room

import (
	"context"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// roomHost 包装 Host，让 GossipSub 的流允许经过有资源上限的 circuit relay。
//
// 群聊消息很小（文本 ≤32000 runes），受限中继的 2 分钟/128KB 额度足够；
// 不加这个包装时，libp2p 会直接拒绝在 limited connection 上开 pubsub 流。
type roomHost struct{ host.Host }

// NewStream 实现 host.Host：在原始 NewStream 上附加 WithAllowLimitedConn。
//
// 参数:
//   - ctx: context.Context — 拨号/开流上下文
//   - p: peer.ID — 目标节点
//   - pids: ...protocol.ID — 协议 ID（GossipSub 的 /meshsub/...）
//
// 返回:
//   - network.Stream — 已建立的流
//   - error — 对端不可达/协议不支持时返回
func (h roomHost) NewStream(ctx context.Context, p peer.ID, pids ...protocol.ID) (network.Stream, error) {
	return h.Host.NewStream(network.WithAllowLimitedConn(ctx, "fst-room"), p, pids...)
}
