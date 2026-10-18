package api

import (
	"errors"
	"net/http"
	"strconv"

	"fst/internal/store"
)

// rooms 返回当前可进入的群聊房间（局域网 + 当前有效分组），已失效的历史分组房间不再返回。
//
// 返回:
//   - any — []store.Room（按最后活跃时间倒序）
//   - error — 群聊服务未启动或查询失败时返回
func (a *API) rooms(*http.Request) (any, error) {
	if a.r == nil {
		return []store.Room{}, errors.New("群聊未启用")
	}
	list, err := a.r.Rooms()
	if list == nil {
		list = []store.Room{}
	}
	return list, err
}

// roomMessages 返回某个房间的本机历史，并把该房间标记为已读。
//
// 参数:
//   - r: *http.Request — 路径 {id}、查询 before/limit
//
// 返回:
//   - any — []store.RoomMessage（时间升序）
//   - error — 群聊未启用或查询失败时返回
func (a *API) roomMessages(r *http.Request) (any, error) {
	if a.r == nil {
		return nil, errors.New("群聊未启用")
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	list, err := a.r.Messages(r.PathValue("id"), before, intQ(r, "limit", 50))
	if list == nil {
		list = []store.RoomMessage{}
	}
	return list, err
}

// sendRoomMessage 发送一条群聊文本（只落本机历史并广播，失败不阻塞本地气泡）。
//
// 参数:
//   - r: *http.Request — 路径 {id}，JSON 体 {content}
//
// 返回:
//   - any — {message}：已落库的群聊消息
//   - error — 房间未加入、正文非法或写库失败时返回
func (a *API) sendRoomMessage(r *http.Request) (any, error) {
	if a.r == nil {
		return nil, errors.New("群聊未启用")
	}
	var in struct {
		Content string `json:"content"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	m, err := a.r.Publish(r.PathValue("id"), in.Content)
	if err != nil {
		return nil, err
	}
	return map[string]any{"message": m}, nil
}
