package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// imageTypes 是媒体端点允许的扩展名 -> Content-Type；非图片一律 404，避免变成通用文件下载器。
var imageTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
}

// media 只读地返回一条已完成传输的图片，供聊天图片气泡直接 <img src> 使用。
//
// 参数:
//   - w: http.ResponseWriter — 响应；成功时写图片字节，失败统一 404（不泄露路径/原因）
//   - r: *http.Request — 路径参数 {id} 为传输记录 ID
//
// 返回:
//   - 无（直接写响应，不走 {code,message,data} 信封）
//
// 安全边界：仅读本机 transfers 表里的记录、仅 done 状态、仅图片扩展名；直接打开记录中的
// 绝对路径，不拼接任何请求参数，因此不存在目录穿越；大小与记录不一致时拒绝。
func (a *API) media(w http.ResponseWriter, r *http.Request) {
	t, ok := a.n.Store.Transfer(r.PathValue("id"))
	if !ok || t.Status != "done" || t.LocalPath == "" {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(filepath.Ext(t.LocalPath))
	ctype, isImage := imageTypes[ext]
	if !isImage {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(t.LocalPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() || st.Size() != t.Size {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, filepath.Base(t.LocalPath), st.ModTime(), f)
}
