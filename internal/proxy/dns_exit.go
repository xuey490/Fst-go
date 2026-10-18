package proxy

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// exitDNSServer 是经出口查询时使用的解析器（出口侧直连，不经本地网络栈）。
const exitDNSServer = "8.8.8.8:53"

// dnsQueryTimeout 单次出口 DNS 查询上限：超过就回 SERVFAIL，让客户端改试下一次而不是无限等待。
const dnsQueryTimeout = 2 * time.Second

// newExitResolver 构造「经代理出口解析」的解析器（支持 A 与 AAAA）。
//
// 参数:
//   - c: *Client — 已连上出口中继的代理客户端（不可为 nil）
//
// 返回:
//   - exitResolver — 每次调用打开一条 UDP 会话向 8.8.8.8:53 发查询并解析应答；超时/失败返回 error
func newExitResolver(c *Client) exitResolver {
	return func(ctx context.Context, host, family string) ([]netip.Addr, error) {
		qctx, cancel := context.WithTimeout(ctx, dnsQueryTimeout)
		defer cancel()
		pc, err := c.DialUDP(qctx, exitDNSServer)
		if err != nil {
			return nil, fmt.Errorf("出口 DNS 会话: %w", err)
		}
		defer pc.Close()
		// 超时/取消时关闭会话，避免 ReadFrom 卡在流上
		stop := context.AfterFunc(qctx, func() { _ = pc.Close() })
		defer stop()
		_ = pc.SetDeadline(time.Now().Add(dnsQueryTimeout))

		query, err := packQuery(host, family)
		if err != nil {
			return nil, err
		}
		if _, err := pc.WriteTo(query, nil); err != nil {
			return nil, fmt.Errorf("发送 DNS 查询: %w", err)
		}
		buf := make([]byte, 1500)
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			return nil, fmt.Errorf("读取 DNS 应答: %w", err)
		}
		return parseAnswer(buf[:n], family)
	}
}

// packQuery 组装一条递归查询 A 或 AAAA 记录的 DNS 报文。
//
// 参数:
//   - host: string — 域名（不带结尾点）
//   - family: string — "ip6" 查 AAAA，其余查 A
//
// 返回:
//   - []byte — 可直接发送的 DNS 报文
//   - error — 域名不合法或序列化失败
func packQuery(host, family string) ([]byte, error) {
	// dnsmessage.NewName 只校验长度，不校验字符集；先把明显不是域名的输入挡掉
	if host == "" || strings.HasPrefix(host, ".") || strings.Contains(host, "..") {
		return nil, fmt.Errorf("域名无效 %q", host)
	}
	for _, r := range host {
		ok := r == '-' || r == '_' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return nil, fmt.Errorf("域名无效 %q", host)
		}
	}
	name, err := dnsmessage.NewName(host + ".")
	if err != nil {
		return nil, fmt.Errorf("域名无效 %q: %w", host, err)
	}
	qtype := dnsmessage.TypeA
	if family == "ip6" {
		qtype = dnsmessage.TypeAAAA
	}
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: uint16(rand.Uint32()), RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: name, Type: qtype, Class: dnsmessage.ClassINET}},
	}
	return msg.Pack()
}

// parseAnswer 从出口返回的 DNS 报文中取出 A/AAAA 记录。
//
// 参数:
//   - raw: []byte — 出口返回的完整 DNS 报文
//   - family: string — "ip6" 只收 AAAA，其余只收 A
//
// 返回:
//   - []netip.Addr — 对应地址族的地址列表；无记录时为空
//   - error — 报文损坏或返回码非成功（例如 SERVFAIL/REFUSED）
func parseAnswer(raw []byte, family string) ([]netip.Addr, error) {
	var p dnsmessage.Parser
	hdr, err := p.Start(raw)
	if err != nil {
		return nil, err
	}
	if hdr.RCode != dnsmessage.RCodeSuccess {
		return nil, fmt.Errorf("出口 DNS 返回 %s", hdr.RCode)
	}
	if _, err := p.AllQuestions(); err != nil {
		return nil, err
	}
	var out []netip.Addr
	for {
		h, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch h.Type {
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return nil, err
			}
			if family != "ip6" {
				out = append(out, netip.AddrFrom4(r.A))
			}
		case dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				return nil, err
			}
			if family == "ip6" {
				out = append(out, netip.AddrFrom16(r.AAAA))
			}
		case dnsmessage.TypeCNAME:
			if _, err := p.CNAMEResource(); err != nil {
				return nil, err
			}
		default:
			if err := p.SkipAnswer(); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
