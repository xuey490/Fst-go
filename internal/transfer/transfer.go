// Package transfer implements file sending/receiving: offer/accept over /fst/msg and
// raw byte streams over /fst/file with resume and SHA-256 verification.
//
// Wire flow for one batch:
//
//	sender --FILE_OFFER(files)--> receiver      (receiver asks its user, or auto-accepts)
//	sender <-FILE_ACCEPT(offsets)- receiver     (offset = bytes already held in <name>.<size>.fstpart)
//	per file, up to N in parallel, one /fst/file stream:
//	  header{batch,file,offset} + raw bytes[offset:size] + sha256(whole file)
//	  receiver verifies, renames the part file and answers FileResult
package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"fst/internal/chat"
	"fst/internal/node"
	"fst/internal/proto"
	"fst/internal/store"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	maxFiles = 1000 // 单批上限：保证 FILE_OFFER 帧远小于 proto.MaxFrame
	// maxFilesTotal 是一次选择可展开的文件总数上限；超过 maxFiles 时内部自动分批
	maxFilesTotal = 20000
	bufSize       = 256 << 10
	ioTimeout     = 60 * time.Second // per-chunk stall limit, refreshed on every write/read
)

type decision struct {
	accept bool
	reason string
}

type inFile struct {
	batch string
	peer  peer.ID
	name  string
	dir   string // 目标目录（文件夹传输时是下载根目录下的子目录）；单文件即下载根目录
	size  int64
}

type Service struct {
	n *node.Node

	mu        sync.Mutex
	offers    map[string]chan decision      // batch id -> waiting receiver handler
	incoming  map[string]*inFile            // accepted file id -> expectation
	active    map[string]context.CancelFunc // running file id -> cancel
	batches   map[string]context.CancelFunc // outgoing batch id -> cancel
	cancelled map[string]bool

	limMu  sync.Mutex
	lim    *limiter // 全局传输限速器；nil 表示不限速
	limKps int64    // 构建 lim 时使用的 KB/s，设置变化后重建
}

func New(n *node.Node) *Service {
	s := &Service{
		n: n, offers: map[string]chan decision{}, incoming: map[string]*inFile{},
		active: map[string]context.CancelFunc{}, batches: map[string]context.CancelFunc{}, cancelled: map[string]bool{},
	}
	n.Handle(proto.TFileOffer, s.onOffer)
	n.Handle(proto.TFileCancel, s.onCancel)
	n.Host.SetStreamHandler(proto.FileProtocol, s.serveFile)
	return s
}

// ---------- settings ----------

func (s *Service) DownloadDir() string {
	def := ""
	if home, err := os.UserHomeDir(); err == nil {
		def = filepath.Join(home, "Downloads", "FST")
	}
	return s.n.Store.Setting("download_dir", def)
}

func (s *Service) concurrency() int {
	c, _ := strconv.Atoi(s.n.Store.Setting("concurrency", "3"))
	if c < 1 {
		c = 1
	}
	if c > 8 {
		c = 8
	}
	return c
}

// limiter 返回当前生效的全局传输限速器（设置 rate_limit_kbps，0 表示不限速）。
//
// 返回:
//   - *limiter — 可为 nil；nil 时调用方不等待
//
// 并发：设置值变化时重建，多个文件共用同一实例以保证总速率。
func (s *Service) limiter() *limiter {
	kps, _ := strconv.ParseInt(strings.TrimSpace(s.n.Store.Setting("rate_limit_kbps", "0")), 10, 64)
	if kps < 0 {
		kps = 0
	}
	if kps > 1<<20 {
		kps = 1 << 20
	}
	s.limMu.Lock()
	defer s.limMu.Unlock()
	if s.lim == nil || s.limKps != kps {
		s.lim = newLimiter(kps * 1024)
		s.limKps = kps
	}
	return s.lim
}

// prehash 表示发送前是否先计算整文件 SHA-256（让接收端秒跳过已有文件）。
//
// 返回:
//   - bool — 设置为 "1" 时为 true；默认关闭，避免大文件多读一遍
func (s *Service) prehash() bool {
	return s.n.Store.Setting("prehash", "0") == "1"
}

// ---------- helpers ----------

func terminal(status string) bool {
	switch status {
	case "done", "failed", "cancelled", "rejected":
		return true
	}
	return false
}

func (s *Service) set(id, status string, done int64, errMsg, sha, path string) {
	_ = s.n.Store.UpdateTransfer(id, status, done, errMsg, sha, path)
	t, ok := s.n.Store.Transfer(id)
	if !ok {
		return
	}
	s.n.Emit("transfer:update", t)
	switch status {
	case "done":
		s.n.Emit("transfer:done", t)
	case "failed":
		s.n.Emit("transfer:failed", t)
	}
}

// failOrCancel records how a file ended, honouring an explicit user cancel.
func (s *Service) failOrCancel(id string, done int64, err error) {
	s.mu.Lock()
	c := s.cancelled[id]
	delete(s.cancelled, id)
	s.mu.Unlock()
	if c {
		s.set(id, "cancelled", done, "", "", "")
		return
	}
	s.set(id, "failed", done, err.Error(), "", "")
}

var badName = strings.NewReplacer("<", "_", ">", "_", ":", "_", "\"", "_", "/", "_", "\\", "_", "|", "_", "?", "_", "*", "_")

// safeName turns a remote-supplied name into a plain file name (no paths, no reserved characters).
func safeName(name string) (string, error) {
	name = badName.Replace(name)
	name = strings.TrimRight(strings.TrimSpace(name), ". ")
	if name == "" || name == "." || name == ".." {
		return "", errors.New("invalid file name")
	}
	if r := []rune(name); len(r) > 200 {
		name = string(r[:200])
	}
	return name, nil
}

func partPath(dir, name string, size int64) string {
	return filepath.Join(dir, fmt.Sprintf("%s.%d.fstpart", name, size))
}

// safeRelPath 清洗对端给出的相对路径，保证落盘位置始终在下载目录之内。
//
// 参数:
//   - rel: string — 协议里的相对路径（斜杠分隔，最后一段是文件名）；空串表示单文件
//
// 返回:
//   - string — 清洗后的斜杠相对路径（逐段替换 Windows 保留字符、截断过长段）；空串原样返回
//   - error — 绝对路径、盘符、`.`/`..`/空段、控制字符或整体超长时返回
func safeRelPath(rel string) (string, error) {
	if rel == "" {
		return "", nil
	}
	s := strings.ReplaceAll(strings.TrimSpace(rel), "\\", "/")
	// 盘符检查独立于当前平台：Linux 上 filepath.VolumeName("C:/x") 为空，这里必须自己拦下
	if s == "" || strings.HasPrefix(s, "/") || filepath.IsAbs(s) || filepath.VolumeName(s) != "" ||
		(len(s) >= 2 && s[1] == ':') {
		return "", errors.New("invalid relative path")
	}
	if len(s) > 512 {
		return "", errors.New("relative path too long")
	}
	parts := strings.Split(s, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", errors.New("invalid path segment")
		}
		for _, r := range p {
			if r < 0x20 || r == 0x7f {
				return "", errors.New("control character in path")
			}
		}
		p = strings.TrimRight(strings.TrimSpace(badName.Replace(p)), ". ")
		if p == "" || p == "." || p == ".." {
			return "", errors.New("invalid path segment")
		}
		if r := []rune(p); len(r) > 120 {
			p = string(r[:120])
		}
		out = append(out, p)
	}
	return strings.Join(out, "/"), nil
}

// localTarget 计算一条传入文件在下载根目录下的目标目录与文件名。
//
// 参数:
//   - base: string — 下载根目录（本地分隔符）
//   - rel: string — 已安全化的斜杠相对路径；空表示单文件直接落在根目录
//   - name: string — 已安全化的文件名（rel 为空时使用）
//
// 返回:
//   - string — 目标目录
//   - string — 目标文件名（rel 非空时取相对路径的最后一段，保证与落盘位置一致）
func localTarget(base, rel, name string) (string, string) {
	if rel == "" {
		return base, name
	}
	return filepath.Join(base, filepath.FromSlash(filepath.Dir(rel))), filepath.Base(rel)
}

func uniqueName(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		return p
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, err := os.Stat(p); err != nil {
			return p
		}
	}
}

// meter counts bytes, throttles progress events and keeps the stream deadline moving.
// It also enforces the optional global rate limit (one shared limiter for all files).
type meter struct {
	s        *Service
	ctx      context.Context
	id       string
	size     int64
	done     int64
	stream   network.Stream
	lim      *limiter
	lastEmit time.Time
	lastDB   time.Time
	lastB    int64
}

// newMeter 创建进度计（同时承载限速等待）。
//
// 参数:
//   - s: *Service — 所属传输服务
//   - ctx: context.Context — 传输上下文，限速等待随它取消
//   - id: string — 传输记录 ID
//   - size: int64 — 文件总字节数
//   - start: int64 — 续传起点（已持有的字节数）
//   - st: network.Stream — 对应的文件流，用于刷新 deadline
//
// 返回:
//   - *meter — 可交给 io.MultiWriter 的进度计数器
func newMeter(s *Service, ctx context.Context, id string, size, start int64, st network.Stream) *meter {
	return &meter{
		s: s, ctx: ctx, id: id, size: size, done: start, stream: st, lim: s.limiter(),
		lastEmit: time.Now(), lastDB: time.Now(), lastB: start,
	}
}

func (m *meter) Write(p []byte) (int, error) {
	if err := m.lim.wait(m.ctx, len(p)); err != nil {
		return 0, err
	}
	m.done += int64(len(p))
	now := time.Now()
	_ = m.stream.SetDeadline(now.Add(ioTimeout))
	if d := now.Sub(m.lastEmit); d >= 100*time.Millisecond {
		speed := float64(m.done-m.lastB) / d.Seconds()
		m.lastEmit, m.lastB = now, m.done
		m.s.n.Emit("transfer:progress", map[string]any{"id": m.id, "bytes_done": m.done, "size": m.size, "speed": int64(speed)})
		if now.Sub(m.lastDB) >= time.Second {
			m.lastDB = now
			_ = m.s.n.Store.UpdateTransfer(m.id, "running", m.done, "", "", "")
		}
	}
	return len(p), nil
}

// ---------- sending ----------

// sendEntry 是一个待发送文件：本地路径、展示名、协议相对路径与大小。
type sendEntry struct {
	path string // 本地绝对路径
	name string // 文件名（展示与落库用）
	rel  string // 斜杠分隔的相对路径；直接选择的单个文件为空
	size int64  // 字节数
}

// expandPaths 展开用户选择的文件与文件夹，得到待发送清单。
//
// 参数:
//   - paths: []string — 文件选择器/拖放给出的本地路径（文件或文件夹）
//
// 返回:
//   - files: []sendEntry — 每个普通文件一项；文件夹内文件的相对路径以所选文件夹名为前缀
//   - dirs: []string — 不含任何普通文件的目录（空文件夹）的相对路径，供接收端还原目录树
//   - skipped: []string — 被跳过的符号链接/特殊文件（相对路径），供 UI 明确提示
//   - err: error — 路径不可读、总量超过 maxFilesTotal 或没有任何可发送内容时返回
//
// 副作用：只读本地目录树；不跟随符号链接，跳过 .fstpart 临时文件。
func expandPaths(paths []string) (files []sendEntry, dirs, skipped []string, err error) {
	dirHasFile := map[string]bool{}
	var allDirs []string
	// addFile 追加一个普通文件，并把它所有祖先目录标记为「非空」
	addFile := func(fp, name string, size int64, rel string) error {
		if len(files) >= maxFilesTotal {
			return fmt.Errorf("文件数量超过 %d 个，请分批发送", maxFilesTotal)
		}
		files = append(files, sendEntry{path: fp, name: name, size: size, rel: rel})
		if rel != "" {
			for d := path.Dir(rel); d != "." && d != "" && d != "/"; d = path.Dir(d) {
				dirHasFile[d] = true
			}
		}
		return nil
	}
	for _, p := range paths {
		st, serr := os.Stat(p)
		if serr != nil {
			return nil, nil, nil, fmt.Errorf("无法读取 %s: %w", p, serr)
		}
		if st.Mode().IsRegular() {
			if aerr := addFile(p, filepath.Base(p), st.Size(), ""); aerr != nil {
				return nil, nil, nil, aerr
			}
			continue
		}
		if !st.IsDir() {
			skipped = append(skipped, filepath.Base(p))
			continue
		}
		root := filepath.Clean(p)
		rootName := filepath.Base(root)
		werr := filepath.WalkDir(root, func(fp string, d os.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			rel, rerr := filepath.Rel(root, fp)
			if rerr != nil {
				return rerr
			}
			relSlash := path.Join(rootName, filepath.ToSlash(rel))
			if d.IsDir() {
				allDirs = append(allDirs, relSlash) // 根目录也登记，空目录才能被还原
				return nil
			}
			if !d.Type().IsRegular() {
				skipped = append(skipped, relSlash) // 符号链接、设备、管道都不跟随
				return nil
			}
			if strings.HasSuffix(d.Name(), ".fstpart") {
				return nil // 断点临时文件不是用户数据
			}
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			return addFile(fp, d.Name(), info.Size(), relSlash)
		})
		if werr != nil {
			return nil, nil, nil, werr
		}
	}
	for _, d := range allDirs {
		if !dirHasFile[d] {
			dirs = append(dirs, d)
		}
	}
	if len(files) == 0 && len(dirs) == 0 {
		if len(skipped) > 0 {
			return nil, nil, nil, fmt.Errorf("没有可发送的文件（已跳过 %d 个符号链接/特殊文件）", len(skipped))
		}
		return nil, nil, nil, errors.New("没有可发送的文件")
	}
	return files, dirs, skipped, nil
}

// SendReport 是一次发送请求的展开结果；文件超过 maxFiles 时会自动拆成多个批次。
type SendReport struct {
	BatchIDs []string `json:"batch_ids"`
	// IDs 是本批创建的全部传输记录 ID（顺序与所选文件一致），聊天图片/文件卡片用它做附件。
	IDs     []string `json:"ids"`
	Files   int      `json:"files"`
	Dirs    int      `json:"dirs"`
	Skipped []string `json:"skipped"`
}

// Send offers the given files/folders to a peer and streams them once accepted. It returns the first batch id.
//
// 参数:
//   - peerID: string — 接收方 PeerID
//   - paths: []string — 本地文件或文件夹路径；文件夹递归展开
//
// 返回:
//   - string — 首个批次 ID（取消/重试按它索引）
//   - error — 设备 ID 无效、对方离线、路径不可读或没有任何可发送内容
func (s *Service) Send(peerID string, paths []string) (string, error) {
	r, err := s.SendReport(peerID, paths)
	if err != nil {
		return "", err
	}
	return r.BatchIDs[0], nil
}

// SendReport 展开 paths 并按每批 maxFiles 个文件自动分批提交（避免超过协议帧上限）。
//
// 参数:
//   - peerID: string — 接收方 PeerID
//   - paths: []string — 本地文件/文件夹路径（可超过 maxFiles 个文件，内部自动分批）
//
// 返回:
//   - SendReport — 批次 ID、文件数、空目录数与被跳过的特殊文件
//   - error — 展开失败或任一批次提交失败；已提交的批次不会回滚
func (s *Service) SendReport(peerID string, paths []string) (SendReport, error) {
	files, dirs, skipped, err := expandPaths(paths)
	if err != nil {
		return SendReport{}, err
	}
	report := SendReport{
		Files: len(files), Dirs: len(dirs), Skipped: skipped,
		BatchIDs: []string{}, IDs: []string{},
	}
	if report.Skipped == nil {
		report.Skipped = []string{}
	}
	first := true
	for {
		n := len(files)
		if n > maxFiles {
			n = maxFiles
		}
		chunk, rest := files[:n], files[n:]
		var chunkDirs []string
		if first {
			chunkDirs = dirs // 空目录只随首批下发，避免重复创建
		}
		id, ids, serr := s.send(peerID, chunk, chunkDirs)
		if serr != nil {
			return report, serr
		}
		report.BatchIDs = append(report.BatchIDs, id)
		report.IDs = append(report.IDs, ids...)
		first = false
		files = rest
		if len(files) == 0 {
			break
		}
	}
	return report, nil
}

// send 把已展开的文件清单与空目录落库并启动发送协程。
//
// 参数:
//   - peerID: string — 接收方 PeerID
//   - entries: []sendEntry — 同一批次内的待发送文件（可为空，表示只发空目录）
//   - dirs: []string — 本批次要还原的空目录相对路径
//
// 返回:
//   - string — 批次 ID
//   - []string — 本批次创建的文件记录 ID（与 entries 顺序一致）
//   - error — 设备 ID 无效、对方离线、数量越界或落库失败时返回
func (s *Service) send(peerID string, entries []sendEntry, dirs []string) (string, []string, error) {
	pid, err := peer.Decode(peerID)
	if err != nil {
		return "", nil, errors.New("无效的设备 ID")
	}
	if !s.n.IsOnline(pid) {
		return "", nil, errors.New("对方不在线")
	}
	if (len(entries) == 0 && len(dirs) == 0) || len(entries) > maxFiles || len(dirs) > maxFiles {
		return "", nil, fmt.Errorf("文件数量需在 1 到 %d 之间", maxFiles)
	}
	batch := chat.NewID()
	rows := make([]store.Transfer, 0, len(entries))
	files := make([]proto.FileInfo, 0, len(entries))
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		id := chat.NewID()
		ids = append(ids, id)
		rows = append(rows, store.Transfer{
			ID: id, BatchID: batch, PeerID: peerID, Direction: "out", FileName: e.name, RelPath: e.rel,
			Size: e.size, LocalPath: e.path, Status: "pending", CreatedAt: store.Now(),
		})
		files = append(files, proto.FileInfo{ID: id, Name: e.name, Size: e.size, RelPath: e.rel})
	}
	for _, r := range rows {
		if err := s.n.Store.InsertTransfer(r); err != nil {
			return "", nil, err
		}
		s.n.Emit("transfer:update", r)
	}
	ctx, cancel := context.WithCancel(s.n.Context())
	s.mu.Lock()
	s.batches[batch] = cancel
	s.mu.Unlock()
	go s.runSend(ctx, pid, batch, rows, files, dirs)
	return batch, ids, nil
}

func (s *Service) runSend(ctx context.Context, pid peer.ID, batch string, rows []store.Transfer, files []proto.FileInfo, dirs []string) {
	defer func() {
		s.mu.Lock()
		if c := s.batches[batch]; c != nil {
			c()
		}
		delete(s.batches, batch)
		s.mu.Unlock()
	}()
	endAll := func(status, msg string) {
		for _, r := range rows {
			if cur, ok := s.n.Store.Transfer(r.ID); ok && !terminal(cur.Status) {
				s.set(r.ID, status, cur.BytesDone, msg, "", "")
			}
		}
	}

	if err := s.n.EnsureConnected(ctx, pid); err != nil && ctx.Err() == nil {
		endAll("failed", "发送请求失败: "+err.Error())
		return
	}
	// 预校验哈希（可选）：多读一遍文件，换来接收端对已有文件的秒跳过
	if s.prehash() {
		for i := range files {
			sum, herr := hashFile(rows[i].LocalPath)
			if herr != nil {
				endAll("failed", "预校验失败: "+herr.Error())
				return
			}
			files[i].SHA256, rows[i].SHA256 = sum, sum
			_ = s.n.Store.SetTransferSHA(rows[i].ID, sum)
		}
	}
	reply, err := s.n.Request(ctx, pid, &proto.Msg{
		Type: proto.TFileOffer, ID: batch, TS: proto.Now(), Files: files, Dirs: dirs,
	}, 11*time.Minute)
	switch {
	case ctx.Err() != nil:
		endAll("cancelled", "")
		return
	case err != nil:
		endAll("failed", "发送请求失败: "+err.Error())
		return
	case reply.Type == proto.TFileReject:
		endAll("rejected", reply.Reason)
		return
	case reply.Type != proto.TFileAccept:
		endAll("failed", "对方返回了未知响应")
		return
	}

	// 接收端已有同摘要文件：直接标记完成，不建立文件流
	skip := make(map[string]bool, len(reply.Skip))
	for _, id := range reply.Skip {
		skip[id] = true
	}
	for _, r := range rows {
		if skip[r.ID] {
			s.set(r.ID, "done", r.Size, "", r.SHA256, "")
		}
	}
	sem := make(chan struct{}, s.concurrency())
	var wg sync.WaitGroup
	for _, r := range rows {
		if skip[r.ID] {
			continue
		}
		off := reply.Offsets[r.ID]
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				s.failOrCancel(r.ID, 0, ctx.Err())
				return
			}
			fctx, fcancel := context.WithCancel(ctx)
			s.mu.Lock()
			s.active[r.ID] = fcancel
			s.mu.Unlock()
			defer func() {
				fcancel()
				s.mu.Lock()
				delete(s.active, r.ID)
				s.mu.Unlock()
			}()
			if cur, _ := s.n.Store.Transfer(r.ID); terminal(cur.Status) {
				return // cancelled while queued
			}
			s.set(r.ID, "running", off, "", "", "")
			if err := s.sendFile(fctx, pid, batch, r, off); err != nil {
				if ctx.Err() != nil {
					s.mu.Lock()
					s.cancelled[r.ID] = true
					s.mu.Unlock()
				}
				s.failOrCancel(r.ID, 0, err)
			}
		}()
	}
	wg.Wait()
}

func (s *Service) sendFile(ctx context.Context, pid peer.ID, batch string, t store.Transfer, offset int64) error {
	f, err := os.Open(t.LocalPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() != t.Size {
		return errors.New("文件在发送前已被修改")
	}
	if offset < 0 || offset > t.Size {
		offset = 0
	}
	// allow relayed connections: with an FST relay (unlimited) it just works; with a limited third-party
	// relay the stream is cut at the cap and the next attempt resumes from the .part offset
	stream, err := s.n.Host.NewStream(network.WithAllowLimitedConn(ctx, "fst"), pid, proto.FileProtocol)
	if err != nil {
		return err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Reset() })
	defer stop()

	_ = stream.SetDeadline(time.Now().Add(ioTimeout))
	if err := proto.WriteFrame(stream, proto.FileHeader{BatchID: batch, FileID: t.ID, Offset: offset}); err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.CopyN(h, f, offset); err != nil { // hash the prefix the receiver already holds
		return err
	}
	m := newMeter(s, ctx, t.ID, t.Size, offset, stream)
	if _, err := io.CopyBuffer(io.MultiWriter(stream, h, m), f, make([]byte, bufSize)); err != nil {
		return err
	}
	sum := h.Sum(nil)
	_ = stream.SetDeadline(time.Now().Add(ioTimeout))
	if _, err := stream.Write(sum); err != nil {
		return err
	}
	_ = stream.CloseWrite()
	_ = stream.SetReadDeadline(time.Now().Add(2 * time.Minute))
	var res proto.FileResult
	if err := proto.ReadFrame(stream, &res); err != nil {
		return err
	}
	if !res.OK {
		return errors.New("对方拒绝: " + res.Error)
	}
	s.set(t.ID, "done", t.Size, "", hex.EncodeToString(sum), "")
	return nil
}

// Retry re-sends the file behind a failed/cancelled/rejected outgoing transfer.
func (s *Service) Retry(id string) (string, error) {
	t, ok := s.n.Store.Transfer(id)
	if !ok || t.Direction != "out" {
		return "", errors.New("只能重试自己发出的传输")
	}
	if !terminal(t.Status) || t.Status == "done" {
		return "", errors.New("该传输无需重试")
	}
	if t.RelPath != "" { // 文件夹里的文件重试时保留原相对路径，避免落到下载根目录
		b, _, err := s.send(t.PeerID, []sendEntry{{path: t.LocalPath, name: t.FileName, rel: t.RelPath, size: t.Size}}, nil)
		return b, err
	}
	return s.Send(t.PeerID, []string{t.LocalPath})
}

// RetryBatch 继续一个发送批次：只重发未完成的文件，已完成的文件不再打扰接收端。
//
// 参数:
//   - batchID: string — 原批次 ID
//
// 返回:
//   - string — 新批次 ID（失败文件按原相对路径与文件名重发）
//   - error — 批次不存在、不属于本机发送、没有可重试文件或对方离线时返回
func (s *Service) RetryBatch(batchID string) (string, error) {
	rows, err := s.n.Store.BatchTransfers(batchID)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", errors.New("批次不存在")
	}
	if rows[0].Direction != "out" {
		return "", errors.New("只能重试自己发出的传输")
	}
	var entries []sendEntry
	for _, r := range rows {
		if r.Status == "done" {
			continue
		}
		entries = append(entries, sendEntry{path: r.LocalPath, name: r.FileName, rel: r.RelPath, size: r.Size})
	}
	if len(entries) == 0 {
		return "", errors.New("该批次没有需要重试的文件")
	}
	if len(entries) > maxFiles {
		entries = entries[:maxFiles]
	}
	b, _, err := s.send(rows[0].PeerID, entries, nil)
	return b, err
}

// hashFile 流式计算整文件 SHA-256（小写十六进制）。
//
// 参数:
//   - path: string — 本地文件路径
//
// 返回:
//   - string — 64 字符十六进制摘要
//   - error — 打开或读取失败时返回
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, bufSize)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------- receiving ----------

// Decide answers a pending incoming offer (batch id).
func (s *Service) Decide(batch string, accept bool) error {
	s.mu.Lock()
	ch := s.offers[batch]
	s.mu.Unlock()
	if ch == nil {
		return errors.New("该请求已失效")
	}
	select {
	case ch <- decision{accept: accept, reason: "对方已拒绝"}:
	default:
	}
	return nil
}

func (s *Service) onOffer(ctx context.Context, from peer.ID, in *proto.Msg) *proto.Msg {
	bad := func(why string) *proto.Msg { return &proto.Msg{Type: proto.TError, Reason: why} }
	if in.ID == "" || (len(in.Files) == 0 && len(in.Dirs) == 0) || len(in.Files) > maxFiles || len(in.Dirs) > maxFiles {
		return bad("bad offer")
	}
	_ = s.n.Store.UpsertPeer(store.Peer{PeerID: from.String()})
	// 空文件夹：与文件同样的清洗规则，接收时按目录树创建
	var dirs []string
	for _, d := range in.Dirs {
		rel, derr := safeRelPath(d)
		if derr != nil || rel == "" {
			return bad("bad directory entry")
		}
		dirs = append(dirs, rel)
	}
	var rows []store.Transfer
	var skip []string
	for _, f := range in.Files {
		name, err := safeName(f.Name)
		if err != nil || f.Size < 0 || f.ID == "" {
			return bad("bad file entry")
		}
		rel, err := safeRelPath(f.RelPath)
		if err != nil {
			return bad("bad relative path")
		}
		// 发送端预计算了摘要且本机已有同摘要文件：直接复用，不再接收字节
		if f.SHA256 != "" {
			if have, ok := s.n.Store.DoneTransferBySHA(from.String(), f.SHA256, f.Size); ok {
				rows = append(rows, store.Transfer{
					ID: f.ID, BatchID: in.ID, PeerID: from.String(), Direction: "in", FileName: name, RelPath: rel,
					Size: f.Size, SHA256: f.SHA256, LocalPath: have.LocalPath, Status: "done",
					BytesDone: f.Size, CreatedAt: store.Now(), FinishedAt: store.Now(),
				})
				skip = append(skip, f.ID)
				continue
			}
		}
		rows = append(rows, store.Transfer{
			ID: f.ID, BatchID: in.ID, PeerID: from.String(), Direction: "in", FileName: name, RelPath: rel,
			Size: f.Size, SHA256: f.SHA256, Status: "pending", CreatedAt: store.Now(),
		})
	}
	ch := make(chan decision, 1)
	s.mu.Lock()
	if _, dup := s.offers[in.ID]; dup {
		s.mu.Unlock()
		return bad("duplicate offer")
	}
	s.offers[in.ID] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.offers, in.ID); s.mu.Unlock() }()

	for _, r := range rows {
		if err := s.n.Store.InsertTransfer(r); err != nil {
			return bad("duplicate file id")
		}
		s.n.Emit("transfer:update", r)
	}

	p, _ := s.n.Store.Peer(from.String())
	auto := p.Trusted || s.n.Store.Setting("auto_accept", "0") == "1"
	s.n.Emit("transfer:offer", map[string]any{
		"batch_id": in.ID, "peer_id": from.String(), "peer_name": s.n.PeerName(from.String()),
		"files": rows, "auto": auto,
	})

	d := decision{accept: auto}
	if !auto {
		select {
		case d = <-ch:
		case <-ctx.Done():
			d = decision{reason: "对方长时间未处理"}
		}
	}
	if !d.accept {
		status := "rejected"
		if d.reason == "cancelled" {
			status = "cancelled"
		}
		for _, r := range rows {
			s.set(r.ID, status, 0, d.reason, "", "")
		}
		return &proto.Msg{Type: proto.TFileReject, ID: in.ID, Reason: d.reason}
	}

	dir := s.DownloadDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		for _, r := range rows {
			s.set(r.ID, "failed", 0, "无法创建下载目录", "", "")
		}
		return &proto.Msg{Type: proto.TFileReject, ID: in.ID, Reason: "接收方无法创建下载目录"}
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(d)), 0o755); err != nil {
			slog.Warn("create incoming folder failed", "dir", d, "err", err)
		}
	}
	offsets := map[string]int64{}
	s.mu.Lock()
	for _, r := range rows {
		if r.Status == "done" {
			continue // 秒跳过：不注册 incoming，也不会收到文件流
		}
		tdir, tname := localTarget(dir, r.RelPath, r.FileName)
		if st, err := os.Stat(partPath(tdir, tname, r.Size)); err == nil && st.Size() <= r.Size {
			offsets[r.ID] = st.Size()
		}
		s.incoming[r.ID] = &inFile{batch: in.ID, peer: from, name: tname, dir: tdir, size: r.Size}
	}
	s.mu.Unlock()
	for _, r := range rows {
		if r.Status == "done" {
			s.n.Emit("transfer:update", r)
			continue
		}
		s.set(r.ID, "accepted", offsets[r.ID], "", "", "")
	}
	// ponytail: accepted-but-never-streamed entries stay in s.incoming until restart; fine for a desktop tool
	return &proto.Msg{Type: proto.TFileAccept, ID: in.ID, Offsets: offsets, Skip: skip}
}

func (s *Service) onCancel(_ context.Context, from peer.ID, in *proto.Msg) *proto.Msg {
	ack := &proto.Msg{Type: proto.TFileCancel, ID: in.ID}
	if in.FileID != "" {
		if t, ok := s.n.Store.Transfer(in.FileID); ok && t.PeerID == from.String() {
			s.cancelFile(in.FileID)
		}
		return ack
	}
	s.mu.Lock()
	ch := s.offers[in.ID]
	bc := s.batches[in.ID]
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- decision{reason: "cancelled"}:
		default:
		}
	}
	if bc != nil {
		if rows, _ := s.n.Store.BatchTransfers(in.ID); len(rows) > 0 && rows[0].PeerID == from.String() {
			bc()
		}
	}
	return ack
}

// cancelFile aborts a running file, or marks a not-yet-started one cancelled.
func (s *Service) cancelFile(id string) {
	s.mu.Lock()
	c := s.active[id]
	if c != nil {
		s.cancelled[id] = true
	}
	s.mu.Unlock()
	if c != nil {
		c()
		return
	}
	if t, ok := s.n.Store.Transfer(id); ok && !terminal(t.Status) {
		s.set(id, "cancelled", t.BytesDone, "", "", "")
	}
}

func (s *Service) serveFile(st network.Stream) {
	defer st.Close()
	from := st.Conn().RemotePeer()
	_ = st.SetReadDeadline(time.Now().Add(30 * time.Second))
	var hdr proto.FileHeader
	if err := proto.ReadFrame(st, &hdr); err != nil {
		st.Reset()
		return
	}
	s.mu.Lock()
	in := s.incoming[hdr.FileID]
	s.mu.Unlock()
	if in == nil || in.peer != from || in.batch != hdr.BatchID {
		_ = proto.WriteFrame(st, proto.FileResult{Error: "unexpected file"})
		return
	}
	defer func() { s.mu.Lock(); delete(s.incoming, hdr.FileID); s.mu.Unlock() }()

	done, err := s.receive(st, hdr.FileID, in, hdr.Offset)
	if err != nil {
		s.failOrCancel(hdr.FileID, done, err)
		_ = st.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_ = proto.WriteFrame(st, proto.FileResult{Error: err.Error()})
		return
	}
	_ = st.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = proto.WriteFrame(st, proto.FileResult{OK: true})
}

func (s *Service) receive(st network.Stream, id string, in *inFile, offset int64) (int64, error) {
	dir := in.dir
	if dir == "" {
		dir = s.DownloadDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { // 文件夹传输：按相对路径逐层创建目标目录
		return 0, err
	}
	part := partPath(dir, in.name, in.size)
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if offset < 0 || offset > in.size || offset > fi.Size() {
		return 0, errors.New("续传偏移不合法")
	}
	if err := f.Truncate(offset); err != nil {
		return 0, err
	}
	h := sha256.New()
	if _, err := io.CopyN(h, f, offset); err != nil { // f is now positioned at offset
		return 0, err
	}

	ctx, cancel := context.WithCancel(s.n.Context())
	defer cancel()
	s.mu.Lock()
	s.active[id] = cancel
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.active, id); s.mu.Unlock() }()
	stop := context.AfterFunc(ctx, func() { st.Reset() })
	defer stop()

	s.set(id, "running", offset, "", "", "")
	m := newMeter(s, ctx, id, in.size, offset, st)
	want := in.size - offset
	n, err := io.CopyBuffer(io.MultiWriter(f, h, m), io.LimitReader(st, want), make([]byte, bufSize))
	if err != nil {
		return offset + n, err
	}
	if n != want {
		return offset + n, io.ErrUnexpectedEOF
	}
	trailer := make([]byte, sha256.Size)
	_ = st.SetReadDeadline(time.Now().Add(ioTimeout))
	if _, err := io.ReadFull(st, trailer); err != nil {
		return in.size, err
	}
	sum := h.Sum(nil)
	if !bytes.Equal(sum, trailer) {
		f.Close()
		_ = os.Remove(part) // corrupt data must not seed the next resume
		return 0, errors.New("SHA-256 校验失败，已丢弃临时文件")
	}
	if err := f.Sync(); err != nil {
		return in.size, err
	}
	f.Close()
	final := uniqueName(dir, in.name)
	if err := os.Rename(part, final); err != nil {
		return in.size, err
	}
	s.set(id, "done", in.size, "", hex.EncodeToString(sum), final)
	return in.size, nil
}

// ---------- cancel (either side) ----------

// Cancel stops a transfer from the local side and tells the peer.
func (s *Service) Cancel(id string) error {
	t, ok := s.n.Store.Transfer(id)
	if !ok {
		return errors.New("传输不存在")
	}
	if terminal(t.Status) {
		return errors.New("传输已结束")
	}
	pid, _ := peer.Decode(t.PeerID)
	msg := &proto.Msg{Type: proto.TFileCancel, ID: t.BatchID, FileID: id}
	switch {
	case t.Status == "pending" && t.Direction == "out":
		msg.FileID = ""
		s.mu.Lock()
		bc := s.batches[t.BatchID]
		s.mu.Unlock()
		if bc != nil {
			bc() // runSend marks every row cancelled
		}
	case t.Status == "pending" && t.Direction == "in":
		_ = s.Decide(t.BatchID, false)
		return nil // the rejection itself informs the sender
	default:
		s.cancelFile(id)
	}
	go func() {
		ctx, cancel := context.WithTimeout(s.n.Context(), 10*time.Second)
		defer cancel()
		_, _ = s.n.Request(ctx, pid, msg, 10*time.Second)
	}()
	return nil
}
