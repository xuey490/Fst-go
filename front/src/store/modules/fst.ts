/**
 * FST 全局状态：本机身份、设备列表、会话/消息、传输进度与入站请求。
 * Go -> UI 通过 Wails 事件推送（peer:* / chat:* / transfer:*），在 init() 中一次性订阅。
 */
import { defineStore } from 'pinia'
import { Events } from '@wailsio/runtime'
import { ElCheckbox, ElMessage, ElMessageBox } from 'element-plus'
import * as api from '@/api/fst'
import { useUserStore } from './user'
import { useMenuStore } from './menu'
import { router } from '@/router'

const byOnline = (a: Fst.Peer, b: Fst.Peer) =>
  Number(b.online) - Number(a.online) || (a.name || a.peer_id).localeCompare(b.name || b.peer_id)

export const useFstStore = defineStore('fstStore', () => {
  const self = ref<Fst.Self>()
  const peers = ref<Fst.Peer[]>([])
  const conversations = ref<Fst.Conversation[]>([])
  const messages = ref<Record<string, Fst.Message[]>>({})
  const activePeer = ref('')
  /** 群聊房间列表与本地历史（按房间 ID 分组） */
  const rooms = ref<Fst.Room[]>([])
  const roomMessages = ref<Record<string, Fst.RoomMessage[]>>({})
  const activeRoom = ref('')
  const progress = ref<Record<string, Fst.Progress>>({})
  const transfers = ref<Record<string, Fst.Transfer>>({})
  /** 代理运行状态（启动结果 + 出口自检），设置页展示 */
  const proxyStatus = ref<Fst.ProxyStatus>()
  /** 高级网络功能状态（默认关闭；解锁入口见 docs/开发文档.md） */
  const features = ref<Fst.Features>({ wan_unlocked: false })
  /** 每次传输状态变化 +1，传输历史页据此刷新表格 */
  const transferTick = ref(0)
  const totalUnread = computed(
    () =>
      conversations.value.reduce((n, c) => n + c.unread, 0) +
      rooms.value.reduce((n, r) => n + r.unread, 0)
  )

  // 聊天菜单未读角标:菜单列表是响应式的,直接改 Chat 路由的 meta(菜单列表晚于首次计算设置,所以也监听它)
  const menu = useMenuStore()
  watch(
    [totalUnread, () => menu.menuList],
    ([n]) => {
      const chat = menu.menuList.find((r) => r.name === 'Chat')
      if (!chat?.meta) return
      chat.meta.showBadge = n > 0
      chat.meta.showTextBadge = n > 99 ? '99+' : n > 0 ? String(n) : ''
    },
    { immediate: true }
  )

  const peerName = (id: string) => {
    const p = peers.value.find((x) => x.peer_id === id)
    return p?.name || id.slice(-8)
  }

  async function loadSelf() {
    self.value = await api.fetchSelf()
    // ponytail: 模板的权限指令读取 buttons，FST 无权限体系，统一放行
    useUserStore().setUserInfo({
      buttons: ['*'],
      roles: [],
      id: 0,
      username: self.value.name,
      email: '',
      phone: ''
    })
  }
  async function loadPeers() {
    peers.value = (await api.fetchPeers()).sort(byOnline)
  }
  async function loadConversations() {
    conversations.value = await api.fetchConversations()
  }

  function upsertPeer(p: Partial<Fst.Peer> & { peer_id: string }) {
    const i = peers.value.findIndex((x) => x.peer_id === p.peer_id)
    if (i >= 0) Object.assign(peers.value[i], p)
    else peers.value.push(p as Fst.Peer)
    peers.value.sort(byOnline)
  }

  function removePeer(peerId: string) {
    peers.value = peers.value.filter((p) => p.peer_id !== peerId)
    conversations.value = conversations.value.filter((c) => c.peer_id !== peerId)
    delete messages.value[peerId]
  }

  const viewing = (id: string) => id === activePeer.value && router.currentRoute.value.name === 'Chat'
  const viewingRoom = (id: string) =>
    id === activeRoom.value && router.currentRoute.value.name === 'Chat'

  function touchConversation(m: Fst.Message) {
    const c = conversations.value.find((x) => x.peer_id === m.peer_id)
    const unread = m.direction === 'in' && !viewing(m.peer_id) ? 1 : 0
    if (c) {
      c.last_content = m.content
      c.last_ts = m.ts
      c.last_kind = m.kind || 'text'
      c.last_recalled = !!m.recalled
      c.unread += unread
    } else {
      conversations.value.push({
        peer_id: m.peer_id,
        last_content: m.content,
        last_ts: m.ts,
        unread,
        last_kind: m.kind || 'text',
        last_recalled: !!m.recalled
      })
    }
    conversations.value.sort((a, b) => b.last_ts - a.last_ts)
  }

  async function openChat(peerId: string) {
    activePeer.value = peerId
    if (!messages.value[peerId]) {
      messages.value[peerId] = (await api.fetchMessages(peerId)).reverse()
    }
    await readChat(peerId)
  }
  async function readChat(peerId: string) {
    const c = conversations.value.find((x) => x.peer_id === peerId)
    if (c?.unread) {
      c.unread = 0
      await api.fetchMarkRead(peerId)
    }
  }
  async function loadOlder(peerId: string) {
    const cur = messages.value[peerId] || []
    const older = (await api.fetchMessages(peerId, cur[0]?.ts || 0)).reverse()
    messages.value[peerId] = [...older, ...cur]
    return older.length
  }

  /**
   * 加载群聊房间列表（局域网 + 当前有效分组）。
   * @returns Promise<void> 无返回
   */
  async function loadRooms() {
    rooms.value = await api.fetchRooms()
    // 分组被屏蔽/更换后，当前房间可能已不可见：清空选中态，避免停留在隐藏房间。
    if (activeRoom.value && !rooms.value.some((r) => r.id === activeRoom.value)) {
      activeRoom.value = ''
      roomMessages.value = {}
    }
  }

  /**
   * 加载高级网络功能状态。
   * @returns Promise<void> 无返回
   */
  async function loadFeatures() {
    features.value = await api.fetchFeatures()
  }

  /**
   * 写入高级网络功能开关（本机持久化，重启后装配生效）。
   * @param enable true 解锁；false 屏蔽
   * @returns Promise<boolean> 是否写入成功；被外部环境强制开启时返回 false
   */
  async function setWanUnlocked(enable: boolean) {
    const r = await api.fetchSetWanUnlocked(enable).catch(() => null)
    if (!r) return false
    features.value = { wan_unlocked: r.wan_unlocked }
    return true
  }

  /**
   * 打开群聊房间并加载本地历史（服务端同时标记已读）。
   * @param roomId 房间 ID
   * @returns Promise<void> 无返回
   */
  async function openRoom(roomId: string) {
    activeRoom.value = roomId
    const list = await api.fetchRoomMessages(roomId)
    roomMessages.value[roomId] = list
    const r = rooms.value.find((x) => x.id === roomId)
    if (r) r.unread = 0
  }

  /**
   * 加载更早的群聊消息。
   * @param roomId 房间 ID
   * @returns Promise<number> 本次新增的条数
   */
  async function loadOlderRoom(roomId: string) {
    const cur = roomMessages.value[roomId] || []
    const older = await api.fetchRoomMessages(roomId, cur[0]?.ts || 0)
    roomMessages.value[roomId] = [...older, ...cur]
    return older.length
  }

  /**
   * 发送一条群聊文本。
   * @param roomId 房间 ID
   * @param content 正文（≤32000 字符）
   * @returns Promise<void> 无返回；消息由 room:message 事件统一入列
   */
  async function sendRoomMessage(roomId: string, content: string) {
    const r = await api.fetchSendRoomMessage(roomId, content)
    const list = (roomMessages.value[roomId] ||= [])
    if (!list.some((x) => x.id === r.message.id)) list.push(r.message)
  }
  /**
   * 发送聊天消息并追加到本地会话。
   * @param peerId 对方 PeerID
   * @param content 正文（图片/文件卡片可为空）
   * @param kind 气泡类型
   * @param attach 附件传输记录 ID
   * @param reply 引用信息；不传表示普通消息
   * @returns Promise<void> 无返回；投递失败时弹出错误提示
   */
  async function sendMessage(
    peerId: string,
    content: string,
    kind: Fst.MessageKind = 'text',
    attach: string[] = [],
    reply?: Fst.Reply
  ) {
    const r = await api.fetchSendMessage(peerId, content, kind, attach, reply)
    ;(messages.value[peerId] ||= []).push(r.message)
    touchConversation(r.message)
    if (r.error) ElMessage.error(r.error)
  }

  /**
   * 撤回自己发送的消息并同步本地状态。
   * @param peerId 会话对端 PeerID
   * @param id 消息 ID
   * @returns Promise<void> 无返回；失败时错误由 http 层提示
   */
  async function recallMessage(peerId: string, id: string) {
    const m = await api.fetchRecallMessage(id)
    const cur = messages.value[peerId]?.find((x) => x.id === id)
    if (cur) {
      cur.recalled = true
      cur.content = m.content
    }
    const c = conversations.value.find((x) => x.peer_id === peerId)
    if (c) c.last_recalled = true
  }

  /**
   * 手动重投一条待发送/失败的私聊消息。
   * @param peerId 会话对端 PeerID
   * @param id 消息 ID
   * @returns Promise<void> 无返回；成功后退避清零，状态由 chat:ack 更新
   */
  async function retryMessage(peerId: string, id: string) {
    const m = await api.fetchRetryMessage(id)
    const cur = messages.value[peerId]?.find((x) => x.id === id)
    if (cur) {
      cur.status = m.status
      cur.attempts = m.attempts
      cur.next_retry = m.next_retry
    }
  }

  // ---- 入站文件请求：弹窗确认 ----
  let openBatch = ''
  async function onOffer(o: Fst.Offer) {
    if (o.auto) return
    openBatch = o.batch_id
    const size = (n: number) => (n / 1048576).toFixed(n > 1048576 ? 1 : 3) + ' MB'
    // 文件名/设备名来自对端，必须转义后再放进 HTML
    const list = o.files
      .slice(0, 6)
      .map((f) => `${escapeHtml(f.file_name)} (${size(f.size)})`)
      .join('<br>')
    const more = o.files.length > 6 ? `<br>… 共 ${o.files.length} 个文件` : ''
    const relayed = peers.value.find((p) => p.peer_id === o.peer_id)?.relayed
    const relayHint = relayed
      ? '<br><span style="color:#E6A23C">当前只经中继连接：若中继限流，文件可能中断，建议直连或使用自建不限量中继。</span>'
      : ''
    try {
      await ElMessageBox.confirm(
        `<b>${escapeHtml(o.peer_name || '未知设备')}</b> 想发送：<br>${list}${more}${relayHint}`,
        '收到文件',
        {
          confirmButtonText: '接收',
          cancelButtonText: '拒绝',
          dangerouslyUseHTMLString: true,
          type: 'info',
          closeOnClickModal: false
        }
      )
      await api.fetchTransferAction(o.batch_id, 'accept')
    } catch (e) {
      if (openBatch === o.batch_id && e === 'cancel') {
        await api.fetchTransferAction(o.batch_id, 'reject').catch(() => {})
      }
    }
    openBatch = ''
  }

  // ---- 关闭按钮(close_action=ask):托盘 / 退出,可记住选择 ----
  let asking = false
  async function onCloseRequest() {
    if (asking) return
    asking = true
    const remember = ref(false)
    let act: 'tray' | 'quit' | '' = ''
    try {
      await ElMessageBox({
        title: '关闭 FST',
        message: () =>
          h('div', [
            h('p', { class: 'mb-2' }, '最小化到托盘后仍可接收消息和文件。'),
            h(ElCheckbox, {
              modelValue: remember.value,
              'onUpdate:modelValue': (v: unknown) => (remember.value = !!v),
              label: '记住我的选择'
            })
          ]),
        showCancelButton: true,
        distinguishCancelAndClose: true, // 右上角 X = 什么都不做
        confirmButtonText: '最小化到托盘',
        cancelButtonText: '直接退出',
        closeOnClickModal: false
      })
      act = 'tray'
    } catch (e) {
      if (e === 'cancel') act = 'quit'
    }
    asking = false
    if (!act) return
    if (remember.value) {
      const s = await api.fetchSettings()
      await api.fetchSaveSettings({ ...s, close_action: act })
    }
    await (act === 'tray' ? api.fetchHideWindow() : api.fetchQuit())
  }

  let inited = false
  function init() {
    if (inited) return
    inited = true
    const on = <T>(name: string, fn: (d: T) => void) => Events.On(name, (e: { data: T }) => fn(e.data))

    on<Fst.Peer>('peer:online', (p) => upsertPeer(p))
    on<Fst.Peer>('peer:updated', (p) => upsertPeer(p))
    on<{ peer_id: string; last_seen: number }>('peer:offline', (p) =>
      upsertPeer({ peer_id: p.peer_id, online: false, last_seen: p.last_seen })
    )
    on<Fst.Message>('chat:message', (m) => {
      ;(messages.value[m.peer_id] ||= []).push(m)
      touchConversation(m)
      if (viewing(m.peer_id)) readChat(m.peer_id)
    })
    on<{ id: string; peer_id: string; status: Fst.Message['status'] }>('chat:ack', (a) => {
      const m = messages.value[a.peer_id]?.find((x) => x.id === a.id)
      if (m) m.status = a.status
    })
    on<{ id: string; peer_id: string }>('chat:recall', (r) => {
      const m = messages.value[r.peer_id]?.find((x) => x.id === r.id)
      if (m) {
        m.recalled = true
        m.content = ''
      }
      const c = conversations.value.find((x) => x.peer_id === r.peer_id)
      if (c) c.last_recalled = true
    })
    on<{ room_id: string; message: Fst.RoomMessage }>('room:message', ({ room_id, message }) => {
      const list = (roomMessages.value[room_id] ||= [])
      if (!list.some((x) => x.id === message.id)) list.push(message)
      let r = rooms.value.find((x) => x.id === room_id)
      if (!r) {
        loadRooms()
        r = rooms.value.find((x) => x.id === room_id)
      }
      if (r) {
        r.last_ts = Math.max(r.last_ts, message.ts)
        r.last_content = message.content
        if (message.sender_id !== self.value?.peer_id && !viewingRoom(room_id)) r.unread++
        if (viewingRoom(room_id)) r.unread = 0
      }
    })
    on<null>('room:update', () => loadRooms())
    on<Fst.Offer>('transfer:offer', onOffer)
    on<Fst.Transfer>('transfer:update', (t) => {
      if (['done', 'failed', 'cancelled', 'rejected'].includes(t.status)) {
        delete transfers.value[t.id]
      } else {
        transfers.value[t.id] = t
      }
      transferTick.value++
      // 发送方撤销：关闭尚未处理的确认框
      if (t.status === 'cancelled' && t.batch_id === openBatch) ElMessageBox.close()
    })
    on<Fst.Progress>('transfer:progress', (p) => {
      progress.value[p.id] = p
      const transfer = transfers.value[p.id]
      if (transfer) transfers.value[p.id] = { ...transfer, bytes_done: p.bytes_done }
    })
    on<Fst.Transfer>('transfer:done', (t) => {
      delete progress.value[t.id]
      delete transfers.value[t.id]
      transferTick.value++
      ElMessage.success(`传输完成：${t.file_name}`)
    })
    on<Fst.Transfer>('transfer:failed', (t) => {
      delete progress.value[t.id]
      delete transfers.value[t.id]
      transferTick.value++
      if (t.status === 'failed') ElMessage.error(`传输失败：${t.file_name}（${t.error}）`)
    })
    on<string>('app:error', (m) => ElMessage.error(m))
    on<null>('app:close-request', onCloseRequest)
    on<Fst.Self['reachability']>('node:reachability', (r) => self.value && (self.value.reachability = r))
    on<Fst.ProxyStatus>('proxy:status', (st) => (proxyStatus.value = st))
    api.fetchProxyStatus().then((st) => (proxyStatus.value = st)).catch(() => {})

    return Promise.all([loadSelf(), loadPeers(), loadConversations(), loadRooms(), loadFeatures()])
  }

  return {
    self, peers, conversations, messages, activePeer, rooms, roomMessages, activeRoom,
    progress, transfers, transferTick, totalUnread, proxyStatus, features,
    peerName, init, loadSelf, loadPeers, loadConversations, openChat, readChat, loadOlder, sendMessage,
    recallMessage, retryMessage, loadRooms, loadFeatures, setWanUnlocked,
    openRoom, loadOlderRoom, sendRoomMessage, removePeer
  }
})

function escapeHtml(s: string) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}
