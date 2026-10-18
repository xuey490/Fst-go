package serve

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// errInvalidToken 是登录失败的业务错误（HTTP 200 + code 400，模板靠 message 提示）。
var errInvalidToken = errors.New("访问令牌不正确")

// uploadRoot 返回 Web 上传临时文件的根目录（数据目录下 web-uploads）。
// ponytail: 上传先落盘再走 transfer.Send（多一次磁盘拷贝但复用度最高）；
// 传输完成前临时文件必须存活，因此只在服务启动时清理超过 24h 的批次目录。
func (s *Server) uploadRoot() string {
	return filepath.Join(s.dataDir, "web-uploads")
}

// cleanUploads 删除 web-uploads 下修改时间超过 24h 的批次目录（服务启动时调用一次）。
//
// 参数:
//   - root: string — web-uploads 根目录；不存在时静默返回
func cleanUploads(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name())
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(p)
		}
	}
}

// saveMultipartFiles 把 multipart 的 files 字段逐个落盘到 root 下新建的批次目录。
//
// 参数:
//   - r: *http.Request — 已 ParseMultipartForm 的请求
//   - root: string — web-uploads 根目录
//
// 返回:
//   - []string — 落盘后的本地绝对路径（顺序与上传一致）
//   - string — 批次目录；失败时为空串且不残留
//   - error — 表单缺 files、文件名为空或写盘失败
func saveMultipartFiles(r *http.Request, root string) ([]string, string, error) {
	form := r.MultipartForm
	if form == nil || len(form.File["files"]) == 0 {
		return nil, "", errors.New("请至少选择一个文件")
	}
	dir, err := os.MkdirTemp(root, time.Now().Format("20060102-150405")+"-")
	if err != nil {
		return nil, "", errors.New("无法创建上传临时目录")
	}
	var paths []string
	for _, fh := range form.File["files"] {
		src, err := fh.Open()
		if err != nil {
			return nil, dir, errors.New("无法读取上传文件")
		}
		// 只取基础名，杜绝 ../ 路径穿越；空名或重名时补序号。
		name := filepath.Base(strings.ReplaceAll(fh.Filename, "\\", "/"))
		if name == "" || name == "." || name == ".." {
			name = "file"
		}
		dst, err := os.CreateTemp(dir, name+"-*")
		if err != nil {
			src.Close()
			return nil, dir, errors.New("无法写入上传文件")
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := src.Close()
		closeErr2 := dst.Close()
		if copyErr != nil || closeErr != nil || closeErr2 != nil {
			return nil, dir, errors.New("无法写入上传文件")
		}
		paths = append(paths, dst.Name())
	}
	return paths, dir, nil
}

// upload 处理 POST /api/fst/transfers/upload：multipart(peer_id, files) 落盘后
// 走既有 transfer.SendReport（确认/信任/进度/断点续传全部复用），响应与
// POST /api/fst/transfers 一致（batch_ids/ids/files/dirs/skipped）。
// 文件内容不经过 JS 内存（浏览器直接流式上传），Go 侧也只流式落盘。
//
// 参数:
//   - w: http.ResponseWriter — 响应流
//   - r: *http.Request — multipart 表单：peer_id 文本字段 + files 文件字段（可多个）
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	root := s.uploadRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		respond(w, nil, errors.New("无法创建上传目录"))
		return
	}
	// 64MB 阈值：以内的文件驻留内存解析，超过部分 Go 自动落盘临时文件。
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		respond(w, nil, errors.New("上传表单无效"))
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	peerID := strings.TrimSpace(r.FormValue("peer_id"))
	if peerID == "" {
		respond(w, nil, errors.New("缺少目标设备"))
		return
	}
	paths, dir, err := saveMultipartFiles(r, root)
	if err != nil {
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
		respond(w, nil, err)
		return
	}
	report, err := s.transfer.SendReport(peerID, paths)
	if err != nil {
		// 未生成传输记录，批次目录无人引用，立即清理。
		_ = os.RemoveAll(dir)
		respond(w, nil, err)
		return
	}
	// ponytail: 传输成功后批次目录保留（send 异步读 LocalPath），由 cleanUploads 按 24h 兜底回收。
	res := map[string]any{
		"batch_ids": report.BatchIDs, "ids": report.IDs, "files": report.Files,
		"dirs": report.Dirs, "skipped": report.Skipped,
	}
	if len(report.BatchIDs) > 0 {
		res["batch_id"] = report.BatchIDs[0]
	}
	respond(w, res, nil)
}

// avatarUpload 处理 POST /api/fst/self/avatar/upload：multipart(file) 校验
// 尺寸与格式后写节点档案（与桌面版 PUT /api/fst/self/avatar 语义一致）。
//
// 参数:
//   - w: http.ResponseWriter — 响应流
//   - r: *http.Request — multipart 表单，file 字段为 PNG/JPEG，≤128KB
func (s *Server) avatarUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		respond(w, nil, errors.New("上传表单无效"))
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	fs := r.MultipartForm.File["file"]
	if len(fs) == 0 {
		respond(w, nil, errors.New("请选择图片文件"))
		return
	}
	src, err := fs[0].Open()
	if err != nil {
		respond(w, nil, errors.New("无法读取图片文件"))
		return
	}
	defer src.Close()
	b, err := io.ReadAll(io.LimitReader(src, maxAvatarUpload+1))
	if err != nil || len(b) == 0 {
		respond(w, nil, errors.New("无法读取图片文件"))
		return
	}
	if len(b) > maxAvatarUpload {
		respond(w, nil, errors.New("头像文件不能超过 128KB（建议 64×64 PNG）"))
		return
	}
	if ct := http.DetectContentType(b); ct != "image/png" && ct != "image/jpeg" {
		respond(w, nil, errors.New("仅支持 PNG 或 JPEG 图片"))
		return
	}
	if err := s.node.SetAvatar(base64.StdEncoding.EncodeToString(b)); err != nil {
		respond(w, nil, err)
		return
	}
	sv := s.node.Self()
	respond(w, map[string]any{"avatar": sv.Avatar}, nil)
}

// maxAvatarUpload 是头像源文件上限（字节），与 api 包的 maxAvatarRaw = 128KB 对齐。
const maxAvatarUpload = 128 << 10
