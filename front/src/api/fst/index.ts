/** FST 后端接口（Go 在同源 /api/fst/* 提供，信封 {code,message,data}） */
import request from '@/utils/http'

const U = '/api/fst'

export const fetchSelf = () => request.get<Fst.Self>({ url: U + '/self' })
export const fetchSetName = (name: string) =>
  request.put<Fst.Self>({ url: U + '/self', data: { name }, showSuccessMessage: true })

export const fetchPeers = () => request.get<Fst.Peer[]>({ url: U + '/peers' })
export const fetchSetTrusted = (id: string, trusted: boolean) =>
  request.put<Fst.Peer>({ url: `${U}/peers/${id}/trusted`, data: { trusted } })
export const fetchDeletePeer = (id: string) =>
  request.del({ url: `${U}/peers/${id}` })
export const fetchConnect = (addr: string) =>
  request.post({ url: U + '/peers/connect', data: { addr }, showSuccessMessage: true })

export const fetchConversations = () =>
  request.get<Fst.Conversation[]>({ url: U + '/conversations' })
export const fetchMessages = (peer_id: string, before = 0, limit = 50) =>
  request.get<Fst.Message[]>({ url: U + '/messages', params: { peer_id, before, limit } })
/**
 * 发送一条聊天消息（文本 / 图片 / 文件卡片），可带引用。
 * @param peer_id 对方 PeerID
 * @param content 正文；图片/文件卡片可为空
 * @param kind 气泡类型：text / image / file
 * @param attach 附件传输记录 ID 列表（≤9）
 * @param reply 引用信息（id + 预览 + 发送者）；不传表示普通消息
 * @returns Promise<{ message: Fst.Message; error?: string }> 已落库消息；投递失败时带 error
 */
export const fetchSendMessage = (
  peer_id: string,
  content: string,
  kind: Fst.MessageKind = 'text',
  attach: string[] = [],
  reply?: Fst.Reply
) =>
  request.post<{ message: Fst.Message; error?: string }>({
    url: U + '/messages',
    data: { peer_id, content, kind, attach, reply },
    showErrorMessage: false
  })
/**
 * 撤回一条 2 分钟内自己发送的消息。
 * @param id 消息 ID
 * @returns Promise<Fst.Message> 撤回后的消息（正文已清空）
 */
export const fetchRecallMessage = (id: string) =>
  request.post<Fst.Message>({ url: `${U}/messages/${id}/recall` })
/**
 * 手动重投一条“待发送/失败”的私聊消息（清零退避后立即尝试）。
 * @param id 消息 ID
 * @returns Promise<Fst.Message> 已重置为 queued 的消息
 */
export const fetchRetryMessage = (id: string) =>
  request.post<Fst.Message>({ url: `${U}/messages/${id}/retry` })

/**
 * 获取当前可进入的群聊房间（局域网 + 当前有效分组；已失效的历史分组房间不返回）。
 * @returns Promise<Fst.Room[]> 房间列表
 */
export const fetchRooms = () => request.get<Fst.Room[]>({ url: U + '/rooms' })
/**
 * 读取高级网络功能是否解锁。
 * @returns Promise<Fst.Features> 当前状态
 */
export const fetchFeatures = () => request.get<Fst.Features>({ url: U + '/features' })
/**
 * 写入高级网络功能开关（存本机 settings，不写 config.yaml，重启后生效）。
 * @param enable true 解锁；false 屏蔽
 * @returns Promise<{ wan_unlocked: boolean; restart_required: boolean }> 写入后的状态
 */
export const fetchSetWanUnlocked = (enable: boolean) =>
  request.post<{ wan_unlocked: boolean; restart_required: boolean }>({
    url: U + '/features',
    data: { enable }
  })
/**
 * 获取某个群聊房间的本机历史（会把该房间标记为已读）。
 * @param room_id 房间 ID
 * @param before 游标时间戳（0 表示最新）
 * @param limit 条数（默认 50）
 * @returns Promise<Fst.RoomMessage[]> 按时间升序的消息
 */
export const fetchRoomMessages = (room_id: string, before = 0, limit = 50) =>
  request.get<Fst.RoomMessage[]>({
    url: `${U}/rooms/${encodeURIComponent(room_id)}/messages`,
    params: { before, limit }
  })
/**
 * 发送一条群聊文本。
 * @param room_id 房间 ID
 * @param content 正文（≤32000 字符）
 * @returns Promise<{ message: Fst.RoomMessage }> 已落库的群聊消息
 */
export const fetchSendRoomMessage = (room_id: string, content: string) =>
  request.post<{ message: Fst.RoomMessage }>({
    url: `${U}/rooms/${encodeURIComponent(room_id)}/messages`,
    data: { content },
    showErrorMessage: true
  })
/**
 * 用本机图片文件设置头像（Go 读文件，字节不经过 JS）。
 * @param path 本地图片路径；空串表示清除头像
 * @returns Promise<Fst.Self> 更新后的本机信息
 */
export const fetchSetAvatar = (path: string) =>
  request.put<Fst.Self>({ url: U + '/self/avatar', data: { path }, showSuccessMessage: true })
export const fetchMarkRead = (peer_id: string) =>
  request.post({ url: U + '/messages/read', data: { peer_id } })

export const fetchTransfers = (params: Fst.TransferQuery) =>
  request.get<Api.Common.PaginatedResponse<Fst.Transfer>>({ url: U + '/transfers', params })
/**
 * 发送文件或文件夹（Go 侧展开目录并自动分批）。
 * @param peer_id 接收方 PeerID
 * @param paths 本地路径列表；文件夹会递归展开
 * @returns Promise<Fst.SendResult> 批次 ID、文件/目录数量与被跳过的特殊文件
 */
export const fetchSendFiles = (peer_id: string, paths: string[]) =>
  request.post<Fst.SendResult>({ url: U + '/transfers', data: { peer_id, paths } })
export const fetchTransferAction = (
  id: string,
  act: 'accept' | 'reject' | 'cancel' | 'retry' | 'reveal'
) => request.post({ url: `${U}/transfers/${id}/${act}` })

/**
 * 继续一个发送批次：只重发未完成的文件。
 * @param batch_id 原批次 ID
 * @returns Promise<{ batch_id: string }> 新批次 ID
 */
export const fetchRetryBatch = (batch_id: string) =>
  request.post<{ batch_id: string }>({ url: `${U}/transfers/batch/${batch_id}/retry` })

/**
 * 清理传输日志：只删勾选的记录，未勾选的保持不动；磁盘文件与聊天引用不动。
 * @param ids 要清理的记录 ID（必填，空数组会被后端拒绝）
 * @returns Promise<{ deleted: number }> 实际删除的记录数
 */
export const fetchClearTransfers = (ids: string[] = []) =>
  request.post<{ deleted: number }>({ url: `${U}/transfers/clear`, data: { ids } })

/**
 * 聊天图片/文件卡片使用的只读媒体地址（同源 GET，字节不经过 JS 逻辑）。
 * @param transfer_id 已完成传输的记录 ID
 * @returns string 可直接用于 <img :src> 的 URL
 */
export const mediaURL = (transfer_id: string) => `${U}/media/${encodeURIComponent(transfer_id)}`

/** 弹出原生文件选择框（字节不经过 JS，只返回路径） */
export const fetchPickFiles = () => request.post<string[]>({ url: U + '/pick-files' })
export const fetchPickDir = () => request.post<string>({ url: U + '/pick-dir' })
/** 弹出原生目录选择框，返回要发送的文件夹路径（字节不经过 JS） */
export const fetchPickFolder = () => request.post<string>({ url: U + '/pick-folder' })

export const fetchSettings = () => request.get<Fst.Settings>({ url: U + '/settings' })
export const fetchSaveSettings = (s: Fst.Settings) =>
  request.put<Fst.Settings>({ url: U + '/settings', data: s, showSuccessMessage: true })
export const fetchProxyStatus = () => request.get<Fst.ProxyStatus>({ url: U + '/proxy/status' })

export const fetchHideWindow = () => request.post({ url: U + '/app/hide' })
export const fetchOpenDownloadDir = () => request.post({ url: U + '/app/open-download-dir' })
export const fetchQuit = () => request.post({ url: U + '/app/quit' })
