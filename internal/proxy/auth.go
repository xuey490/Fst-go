package proxy

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// tokenEntry 是一个出口令牌的 SHA-256 与过期时间（unix ms，0 不过期）。
type tokenEntry struct {
	hash    [32]byte
	expires int64
}

// TokenSet 是中继出口令牌集合；校验与存储都不保留明文令牌。
type TokenSet struct {
	entries []tokenEntry
}

// ParseTokens 解析令牌文件内容（每行 `token[:expiry]`，空行与 # 注释忽略）。
//
// 参数:
//   - r: io.Reader — 令牌文件内容；expiry 支持 RFC3339 或 Unix 秒
//
// 返回:
//   - *TokenSet — 只含 SHA-256 的令牌集合
//   - error — 行格式错误、令牌为空、过期时间无效或令牌重复
func ParseTokens(r io.Reader) (*TokenSet, error) {
	set := &TokenSet{}
	seen := map[[32]byte]bool{}
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		parts := strings.SplitN(text, ":", 2)
		token := strings.TrimSpace(parts[0])
		if token == "" {
			return nil, fmt.Errorf("令牌文件第 %d 行令牌为空", line)
		}
		entry := tokenEntry{hash: sha256.Sum256([]byte(token))}
		if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
			ts, err := tokenExpiry(parts[1])
			if err != nil {
				return nil, fmt.Errorf("令牌文件第 %d 行过期时间无效: %w", line, err)
			}
			entry.expires = ts
		}
		if seen[entry.hash] {
			return nil, fmt.Errorf("令牌文件第 %d 行令牌重复", line)
		}
		seen[entry.hash] = true
		set.entries = append(set.entries, entry)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return set, nil
}

// ParseTokenFile 读取并解析令牌文件。
//
// 参数:
//   - path: string — 文件路径（-exit-token）
//
// 返回:
//   - *TokenSet — 解析结果
//   - error — 打开/读取/解析失败
func ParseTokenFile(path string) (*TokenSet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseTokens(f)
}

// Len 返回令牌数量（中继启动日志用）。
//
// 返回:
//   - int — 有效条目数
func (t *TokenSet) Len() int {
	if t == nil {
		return 0
	}
	return len(t.entries)
}

// Valid 常量时间校验令牌是否有效且未过期。
//
// 参数:
//   - token: string — 终端提交的明文令牌
//   - now: time.Time — 当前时间（测试可注入）
//
// 返回:
//   - bool — 命中任一未过期令牌时为 true
func (t *TokenSet) Valid(token string, now time.Time) bool {
	if t == nil || len(t.entries) == 0 || token == "" {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	ok := false
	for _, e := range t.entries {
		match := subtle.ConstantTimeCompare(sum[:], e.hash[:]) == 1
		live := e.expires == 0 || now.UnixMilli() <= e.expires
		// 不使用提前 return，避免比较次数泄露命中位置。
		ok = ok || (match && live)
	}
	return ok
}

// tokenExpiry 解析令牌过期时间（RFC3339 或 Unix 秒），统一成 unix ms。
//
// 参数:
//   - text: string — 原始时间文本
//
// 返回:
//   - int64 — unix 毫秒
//   - error — 两种格式都不匹配时返回
func tokenExpiry(text string) (int64, error) {
	text = strings.TrimSpace(text)
	if ts, err := time.Parse(time.RFC3339, text); err == nil {
		return ts.UnixMilli(), nil
	}
	if n, err := strconv.ParseInt(text, 10, 64); err == nil && n > 0 {
		return n * 1000, nil
	}
	return 0, fmt.Errorf("需要 RFC3339 或 Unix 秒: %q", text)
}
