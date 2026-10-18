/** FST 后端（Go /api/fst/*）数据结构 */
declare namespace Fst {
  interface Self {
    peer_id: string
    name: string
    platform: string
    version: string
    port: number
    addrs: string[]
    wan: boolean
    reachability: 'unknown' | 'public' | 'private'
    download_dir: string
    /** 本机头像的裸 base64（≤16KB）；空串表示未设置 */
    avatar?: string
  }
  /** 高级网络功能状态（本机持久化，不写 config.yaml） */
  interface Features {
    /** 是否已解锁跨网络/代理与分组群聊 */
    wan_unlocked: boolean
  }
  interface Peer {
    peer_id: string
    name: string
    platform: string
    ip: string
    addrs: string[]
    online: boolean
    relayed: boolean
    is_relay: boolean
    trusted: boolean
    first_seen: number
    last_seen: number
    /** HELLO 学到的 base64 小头像；空串表示没有 */
    avatar?: string
  }
  type MessageKind = 'text' | 'image' | 'file'
  interface Message {
    id: string
    peer_id: string
    direction: 'in' | 'out'
    content: string
    status: 'sending' | 'sent' | 'delivered' | 'failed' | 'queued'
    ts: number
    read: boolean
    /** 气泡类型：text（默认）/ image / file */
    kind: MessageKind
    /** 该消息引用的传输记录 ID（图片或文件卡片） */
    attach: string[]
    /** 是否已撤回 */
    recalled: boolean
    /** 离线队列已失败次数（仅 queued/failed 有意义） */
    attempts?: number
    /** 下次自动重试时间（unix ms；0 表示立即可投） */
    next_retry?: number
    /** 引用（回复）的目标消息 ID；空串表示普通消息 */
    reply_id?: string
    /** 被引用正文的纯文本预览（≤200 字符） */
    reply_preview?: string
    /** 被引用消息的发送者昵称 */
    reply_sender?: string
  }
  /** 引用（回复）信息：发送时随正文一起提交 */
  interface Reply {
    id: string
    preview: string
    sender: string
  }
  /** 群聊房间（局域网或中继分组自动房间） */
  interface Room {
    id: string
    name: string
    kind: 'lan' | 'group'
    last_ts: number
    last_content: string
    unread: number
    /** 当前是否已订阅（分组失效时会退出） */
    joined: boolean
  }
  /** 群聊消息（只存本机，不做跨节点历史同步） */
  interface RoomMessage {
    id: string
    room_id: string
    sender_id: string
    sender_name: string
    content: string
    ts: number
    read: boolean
  }
  interface Conversation {
    peer_id: string
    last_content: string
    last_ts: number
    unread: number
    last_kind: MessageKind
    last_recalled: boolean
  }
  type TransferStatus =
    | 'pending'
    | 'accepted'
    | 'running'
    | 'done'
    | 'failed'
    | 'cancelled'
    | 'rejected'
  interface Transfer {
    id: string
    batch_id: string
    peer_id: string
    direction: 'in' | 'out'
    file_name: string
    /** 文件夹传输时相对下载根目录的路径（斜杠分隔）；单文件为空串 */
    rel_path: string
    size: number
    /** 整文件 SHA-256（发送端预校验或接收完成后写入） */
    sha256: string
    local_path: string
    status: TransferStatus
    bytes_done: number
    error: string
    created_at: number
    finished_at: number
  }
  interface Progress {
    id: string
    bytes_done: number
    size: number
    speed: number
  }
  interface Offer {
    batch_id: string
    peer_id: string
    peer_name: string
    files: Transfer[]
    auto: boolean
  }
  interface Settings {
    download_dir: string
    concurrency: number
    auto_accept: boolean
    autostart: boolean
    single_instance: boolean
    close_action: 'ask' | 'tray' | 'quit'
    /** 全局传输限速 KB/s；0 表示不限速 */
    rate_limit_kbps: number
    /** 发送前预计算 SHA-256，接收端可秒跳过已有文件 */
    prehash: boolean
    wan_enabled: boolean
    /** 中继分组密钥；留空为不分组（保存后立即重连中继） */
    group: string
    relays: string[]
    config_path: string
    proxy_enable: boolean
    proxy_mode: 'split' | 'global'
    proxy_exit: string
    proxy_routes: string[]
    proxy_domains: string[]
    /** 出口令牌（中继 -exit-token）；空串表示不携带（重启生效） */
    proxy_token: string
    /** 打开 TUN IPv6 栈与 AAAA 代理解析（实验，重启生效） */
    proxy_ipv6: boolean
    /** 是否启动本机 SOCKS5 入站（重启生效）；关闭时下面的参数不生效 */
    socks_enable: boolean
    /** 本机 SOCKS5 入站账号；非空即启用 RFC1929（重启生效） */
    socks_user: string
    socks_pass: string
    /** 允许的来源 CIDR（空表示仅回环） */
    socks_allow: string[]
    socks_listen: string
    /** 全局 / 单 IP 并发上限；0 表示不限 */
    socks_max_conn: number
    socks_max_per_ip: number
    /** 中继回包里的分组状态（只读） */
    relay_group_name: string
    /** unix ms；0 表示不过期 */
    relay_group_expires: number
    relay_group_error: string
  }
  interface ProxyStatus {
    enabled: boolean
    running: boolean
    mode?: 'split' | 'global'
    exit_peer?: string
    socks_addr?: string
    /** "" 未知 | "ok" 出口连通 | 其它 = 自检失败原因 */
    probe?: string
    /** 启动失败原因 */
    error?: string
  }
  /** POST /transfers 的返回：文件超过 1000 个时会有多个批次 */
  interface SendResult {
    batch_id?: string
    batch_ids: string[]
    /** 本批创建的传输记录 ID（顺序与所选文件一致），聊天图片/文件卡片用作附件 */
    ids: string[]
    files: number
    dirs: number
    /** 被跳过的符号链接/特殊文件（相对路径） */
    skipped: string[]
  }
  interface TransferQuery {
    page?: number
    limit?: number
    keyword?: string
    direction?: string
    status?: string
    peer_id?: string
  }
}