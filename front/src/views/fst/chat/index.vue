<!-- 聊天页：左侧设备/会话列表，右侧消息流 -->
<template>
  <div class="flex h-[calc(100vh-120px)] p-4 gap-4">
    <ElCard shadow="never" class="w-64 shrink-0 overflow-auto" body-class="!p-2">
      <div class="flex gap-1 p-1 mb-2 rounded bg-g-200">
        <button
          type="button"
          class="flex-1 py-1 rounded text-xs transition-colors"
          :class="mode === 'private' ? 'bg-white text-theme shadow-sm' : 'text-g-600'"
          @click="switchMode('private')"
        >
          {{ t('fst.chat.private') }}
        </button>
        <button
          type="button"
          class="flex-1 py-1 rounded text-xs transition-colors"
          :class="mode === 'group' ? 'bg-white text-theme shadow-sm' : 'text-g-600'"
          @click="switchMode('group')"
        >
          {{ t('fst.chat.group') }}
        </button>
      </div>
      <template v-if="mode === 'private'">
        <div
          v-for="p in contacts"
          :key="p.peer_id"
          class="flex items-center gap-2 p-2 rounded cursor-pointer hover:bg-g-200"
          :class="{ 'bg-g-200': p.peer_id === fst.activePeer }"
          @click="select(p.peer_id)"
        >
          <ElBadge is-dot :type="p.online ? 'success' : 'info'">
            <ElAvatar :size="24" :src="avatarSrc(p.avatar)">
              <ArtSvgIcon icon="ri:user-line" />
            </ElAvatar>
          </ElBadge>
          <div class="min-w-0 flex-1">
            <div class="truncate">{{ p.name || p.peer_id.slice(-8) }}</div>
            <div class="text-xs text-g-600 truncate">{{ preview(p) }}</div>
          </div>
          <ElBadge v-if="p.unread" :value="p.unread" />
        </div>
        <ElEmpty v-if="!contacts.length" :description="t('fst.chat.noDevices')" :image-size="60" />
      </template>
      <template v-else>
        <button
          v-for="r in fst.rooms"
          :key="r.id"
          type="button"
          class="w-full flex items-center gap-2 p-2 rounded text-left hover:bg-g-200"
          :class="{ 'bg-g-200': r.id === fst.activeRoom }"
          @click="selectRoom(r.id)"
        >
          <div class="w-8 h-8 rounded-lg flex-cc bg-theme/10 text-theme shrink-0">
            <ArtSvgIcon :icon="r.kind === 'lan' ? 'ri:wifi-line' : 'ri:team-line'" />
          </div>
          <div class="min-w-0 flex-1">
            <div class="truncate flex items-center gap-1">
              {{ r.name }}
              <span class="text-[10px] px-1 rounded bg-g-300 text-g-600">{{ roomKind(r) }}</span>
              <span v-if="!r.joined" class="text-[10px] px-1 rounded bg-g-300 text-g-500">
                {{ t('fst.chat.roomHistoryTag') }}
              </span>
            </div>
            <div class="text-xs text-g-600 truncate">{{ r.last_content || t('fst.chat.roomEmpty') }}</div>
          </div>
          <ElBadge v-if="r.unread" :value="r.unread" />
        </button>
        <ElEmpty v-if="!fst.rooms.length" :description="t('fst.chat.noRooms')" :image-size="60" />
      </template>
    </ElCard>

    <ElCard shadow="never" class="flex-1 min-w-0 flex flex-col" body-class="!p-0 flex flex-col h-full">
      <template v-if="mode === 'private' && fst.activePeer">
        <div class="px-4 py-3 border-b border-g-300 font-medium flex items-center gap-2">
          <ElAvatar :size="28" :src="avatarSrc(activeAvatar)">
            <ArtSvgIcon icon="ri:user-line" />
          </ElAvatar>
          {{ fst.peerName(fst.activePeer) }}
          <span v-if="!online" class="text-xs text-g-600 ml-2">{{ t('fst.chat.offlineHint') }}</span>
        </div>
        <div ref="box" class="flex-1 overflow-auto p-4">
          <div class="text-center mb-2">
            <ElLink v-if="more" type="primary" @click="older">{{ t('fst.chat.loadOlder') }}</ElLink>
          </div>
          <div
            v-for="m in msgs"
            :key="m.id"
            class="flex mb-3 group"
            :class="m.direction === 'out' ? 'justify-end' : 'justify-start'"
          >
            <div class="relative max-w-[70%]">
              <MessageActions
                v-if="!m.recalled"
                :can-copy="m.kind === 'text'"
                :can-recall="canRecall(m)"
                :recall-reason="recallReason(m)"
                @quote="startQuote(m)"
                @copy="copyMessage(m)"
                @recall="recall(m)"
              />
              <div
                v-if="m.reply_id"
                class="mb-1 rounded border-l-2 border-theme/60 bg-g-200/80 px-2 py-1 text-xs text-g-600"
              >
                <div class="font-medium text-g-500">
                  {{ m.reply_sender || fst.peerName(m.peer_id) }}
                </div>
                <div class="truncate">{{ m.reply_preview }}</div>
              </div>
              <div v-if="m.recalled" class="text-xs text-g-500 italic px-1">
                {{ t('fst.chat.recalled') }}
              </div>
              <div
                v-else-if="m.kind === 'image'"
                class="flex flex-wrap gap-1"
                :class="m.direction === 'out' ? 'justify-end' : 'justify-start'"
              >
                <ElImage
                  v-for="(id, i) in m.attach"
                  :key="id + ':' + mediaTick"
                  :src="mediaURL(id)"
                  :preview-src-list="m.attach.map(mediaURL)"
                  :initial-index="i"
                  fit="cover"
                  class="w-36 h-36 rounded"
                >
                  <template #error>
                    <div class="w-36 h-36 flex-cc bg-g-200 rounded text-xs text-g-500">
                      {{ t('fst.chat.imagePending') }}
                    </div>
                  </template>
                </ElImage>
              </div>
              <div
                v-else-if="m.kind === 'file'"
                class="w-[260px] px-3 py-2 rounded-lg bg-g-200"
                :class="{ 'bg-theme/10': m.direction === 'out' }"
              >
                <div class="flex items-center gap-2">
                  <ArtSvgIcon icon="ri:file-3-line" class="text-2xl shrink-0" />
                  <div class="min-w-0 flex-1">
                    <div class="text-sm truncate">{{ fileTitle(m) }}</div>
                    <div class="text-xs text-g-500 truncate">{{ fileMeta(m) }}</div>
                  </div>
                  <ElLink type="primary" @click="revealTransfer(m.attach[0])">{{
                    t('fst.transfer.reveal')
                  }}</ElLink>
                </div>
                <ElProgress
                  v-if="filePercent(m) !== null"
                  :percentage="filePercent(m) || 0"
                  :show-text="false"
                  :stroke-width="2"
                  class="mt-1"
                />
              </div>
              <div
                v-else
                class="message-content px-3 py-2 rounded-lg break-words"
                :class="m.direction === 'out' ? 'bg-theme text-white' : 'bg-g-200'"
                v-html="renderMessage(m.content)"
              />
              <div
                class="text-xs text-g-500 mt-1 flex gap-2"
                :class="{ 'justify-end': m.direction === 'out' }"
              >
                <span>{{ time(m.ts) }}</span>
                <span
                  v-if="m.direction === 'out' && !m.recalled"
                  :class="{
                    'text-red-500': m.status === 'failed',
                    'text-g-500': m.status === 'queued'
                  }"
                >
                  {{ statusText(m.status) }}
                </span>
                <ElLink v-if="canRetry(m)" type="primary" @click="retry(m)">{{
                  t('fst.chat.retry')
                }}</ElLink>
              </div>
            </div>
          </div>
        </div>
        <div class="p-3 border-t border-g-300">
          <div
            v-if="quoting"
            class="mb-2 flex items-start gap-2 rounded border border-g-300 bg-g-200 px-2 py-1 text-xs"
          >
            <div class="min-w-0 flex-1">
              <div class="text-g-500">{{ t('fst.chat.quoting') }} {{ quoteSender }}</div>
              <div class="truncate">{{ quotePreview }}</div>
            </div>
            <button
              type="button"
              class="text-g-500 hover:text-theme cursor-pointer"
              :title="t('fst.chat.cancelQuote')"
              @click="quoting = null"
            >
              ✕
            </button>
          </div>
          <div class="chat-editor border border-g-300 rounded overflow-hidden">
            <Toolbar
              :editor="editorRef"
              :default-config="toolbarConfig"
              mode="default"
            />
            <Editor
              v-model="text"
              :default-config="editorConfig"
              mode="default"
              @on-created="handleCreated"
            />
          </div>
          <div class="flex justify-between items-center mt-2">
            <div class="flex items-center gap-2">
              <ElButton :disabled="!online" @click="sendImages">
                <ArtSvgIcon icon="ri:image-add-line" class="mr-1" />
                {{ t('fst.chat.sendImage') }}
              </ElButton>
              <ElButton :disabled="!online" @click="sendFiles">
                <ArtSvgIcon icon="ri:file-3-line" class="mr-1" />
                {{ t('fst.chat.sendFile') }}
              </ElButton>
              <span v-if="plainLen > 3000" class="text-xs text-g-500">{{ plainLen }}/32000</span>
            </div>
            <ElButton type="primary" :disabled="!hasMessage" @click="send">{{
              t('fst.chat.send')
            }}</ElButton>
          </div>
        </div>
      </template>

      <template v-else-if="mode === 'group' && fst.activeRoom">
        <div class="px-4 py-3 border-b border-g-300 flex items-center gap-2">
          <div class="w-8 h-8 rounded-lg flex-cc bg-theme/10 text-theme">
            <ArtSvgIcon :icon="activeRoomInfo?.kind === 'lan' ? 'ri:wifi-line' : 'ri:team-line'" />
          </div>
          <div class="min-w-0">
            <div class="font-medium truncate">{{ activeRoomInfo?.name || t('fst.chat.group') }}</div>
            <div v-if="activeRoomInfo?.joined" class="text-xs text-g-500">
              {{ roomKind(activeRoomInfo) }} · {{ t('fst.chat.roomLocalHint') }}
            </div>
            <div v-else class="text-xs text-orange-500">{{ t('fst.chat.roomArchived') }}</div>
          </div>
        </div>
        <div ref="groupBox" class="flex-1 overflow-auto p-4">
          <div
            v-for="m in groupMsgs"
            :key="m.id"
            class="flex gap-2 mb-3"
            :class="m.sender_id === fst.self?.peer_id ? 'justify-end' : 'justify-start'"
          >
            <ElAvatar
              v-if="m.sender_id !== fst.self?.peer_id"
              :size="28"
              :src="avatarSrc(peerAvatar(m.sender_id))"
              class="shrink-0"
            >
              {{ (m.sender_name || '?').slice(0, 1) }}
            </ElAvatar>
            <div class="max-w-[70%]">
              <div
                v-if="m.sender_id !== fst.self?.peer_id"
                class="text-xs text-g-500 mb-1"
              >
                {{ m.sender_name || m.sender_id.slice(-8) }}
              </div>
              <div
                class="px-3 py-2 rounded-lg break-words whitespace-pre-wrap"
                :class="
                  m.sender_id === fst.self?.peer_id ? 'bg-theme text-white' : 'bg-g-200'
                "
              >
                {{ m.content }}
              </div>
              <div
                class="text-xs text-g-500 mt-1"
                :class="{ 'text-right': m.sender_id === fst.self?.peer_id }"
              >
                {{ time(m.ts) }}
              </div>
            </div>
          </div>
          <ElEmpty
            v-if="!groupMsgs.length"
            :description="t('fst.chat.roomWelcome')"
            :image-size="60"
          />
        </div>
        <div v-if="activeRoomInfo?.joined" class="p-3 border-t border-g-300">
          <ElInput
            v-model="groupText"
            type="textarea"
            :rows="2"
            resize="none"
            :maxlength="32000"
            :placeholder="t('fst.chat.roomPlaceholder')"
            @keydown.enter.exact.prevent="sendGroup"
          />
          <div class="flex justify-between items-center mt-2">
            <span class="text-xs text-g-500">{{ groupLen }}/32000</span>
            <ElButton type="primary" :disabled="!groupText.trim()" @click="sendGroup">
              {{ t('fst.chat.send') }}
            </ElButton>
          </div>
        </div>
        <div v-else class="p-3 border-t border-g-300 flex items-center gap-2 text-xs text-g-500">
          <ArtSvgIcon icon="ri:lock-line" />
          {{ t('fst.chat.roomArchived') }}
        </div>
      </template>

      <ElEmpty
        v-else
        :description="mode === 'group' ? t('fst.chat.pickRoom') : t('fst.chat.pickDevice')"
        class="m-auto"
      />
    </ElCard>
  </div>
</template>

<script setup lang="ts">
  import { Editor, Toolbar } from '@wangeditor/editor-for-vue'
  import { IDomEditor } from '@wangeditor/editor'
  import '@wangeditor/editor/dist/css/style.css'
  import DOMPurify from 'dompurify'
  import { fetchPickFiles, fetchSendFiles, fetchTransferAction, mediaURL } from '@/api/fst'
  import { useFstStore } from '@/store/modules/fst'
  import { ElMessage, ElMessageBox } from 'element-plus'
  import { useI18n } from 'vue-i18n'
  import { avatarURL, formatBytes } from '@/utils/fst'
  import MessageActions from './MessageActions.vue'

  defineOptions({ name: 'Chat' })
  const fst = useFstStore()
  const route = useRoute()
  const { t } = useI18n()
  const text = ref('')
  const editorRef = shallowRef<IDomEditor>()
  const box = ref<HTMLElement>()
  const groupBox = ref<HTMLElement>()
  const more = ref(true)
  /** 私聊 / 群聊双模式 */
  const mode = ref<'private' | 'group'>('private')
  const groupText = ref('')
  const groupMsgs = computed(() => fst.roomMessages[fst.activeRoom] || [])
  const activeRoomInfo = computed(() => fst.rooms.find((r) => r.id === fst.activeRoom))
  const groupLen = computed(() => Array.from(groupText.value).length)
  /** 正在引用的私聊消息；null 表示普通发送 */
  const quoting = ref<Fst.Message | null>(null)
  /** 引用块的发送者名：自己发的显示本机昵称 */
  const quoteSender = computed(() =>
    quoting.value
      ? quoting.value.direction === 'out'
        ? fst.self?.name || t('fst.chat.me')
        : fst.peerName(quoting.value.peer_id)
      : ''
  )
  /** 引用块的正文预览：富文本去标签、图片/文件用占位词，最多 200 字符 */
  const quotePreview = computed(() => {
    const m = quoting.value
    if (!m) return ''
    if (m.kind === 'image') return t('fst.chat.previewImage')
    if (m.kind === 'file') return t('fst.chat.previewFile')
    const plain = m.content
      .replace(/<[^>]*>/g, '')
      .replace(/&nbsp;/g, ' ')
      .trim()
    return Array.from(plain).slice(0, 200).join('')
  })
  /** 撤回时间窗，与 Go chat.recallWindow 一致（2 分钟） */
  const RECALL_WINDOW = 2 * 60 * 1000
  /** 单条消息最多引用的传输数量，与 Go chat.maxAttach 一致 */
  const MAX_ATTACH = 9
  /** 消息状态 -> i18n 词条键 */
  const statusKey: Record<string, string> = {
    sending: 'fst.chat.statusSending',
    sent: 'fst.chat.statusSent',
    delivered: 'fst.chat.statusDelivered',
    failed: 'fst.chat.statusFailed',
    queued: 'fst.chat.statusQueued'
  }
  /**
   * 取消息发送状态文案。
   * @param s 后端返回的状态（sending/sent/delivered/failed）
   * @returns 当前语言下的状态文案；未知状态原样返回
   */
  const statusText = (s: string) => (statusKey[s] ? t(statusKey[s]) : s)
  const toolbarConfig = {
    toolbarKeys: [
      'bold',
      'italic',
      'underline',
      'color',
      'bulletedList',
      'numberedList',
      'insertLink',
      'undo',
      'redo'
    ]
  }
  const editorConfig = {
    placeholder: t('fst.chat.editorPlaceholder'),
    autoFocus: false
  }

  // 有会话或在线的设备都可聊天；按最近消息排序
  const contacts = computed(() => {
    const conv = new Map(fst.conversations.map((c) => [c.peer_id, c]))
    return fst.peers
      .map((p) => {
        const c = conv.get(p.peer_id)
        return {
          ...p,
          last: c?.last_content || '',
          lastKind: c?.last_kind || 'text',
          lastRecalled: !!c?.last_recalled,
          unread: c?.unread || 0,
          ts: c?.last_ts || 0
        }
      })
      .filter((p) => p.online || p.ts)
      .sort((a, b) => b.ts - a.ts || Number(b.online) - Number(a.online))
  })
  const msgs = computed(() => fst.messages[fst.activePeer] || [])
  const online = computed(() => fst.peers.find((p) => p.peer_id === fst.activePeer)?.online)
  /** 当前会话对端的 base64 头像，用于会话头部 */
  const activeAvatar = computed(() => fst.peers.find((p) => p.peer_id === fst.activePeer)?.avatar)
  /** 编辑器 HTML 去掉标签与 &nbsp; 后的纯文本（判空与计数共用） */
  const plainText = computed(() =>
    text.value
      .replace(/<[^>]*>/g, '')
      .replace(/&nbsp;/g, ' ')
      .trim()
  )
  const hasMessage = computed(() => !!plainText.value)
  /** 纯文本字符数，按码点计（与 Go 的 rune 上限 32000 对齐） */
  const plainLen = computed(() => Array.from(plainText.value).length)
  /** 头像 base64 -> data URL（无头像时为空串，ElAvatar 回退插槽图标） */
  const avatarSrc = avatarURL
  const time = (ts: number) => new Date(ts).toLocaleString()
  const renderMessage = (content: string) =>
    DOMPurify.sanitize(content, { USE_PROFILES: { html: true } })
  /**
   * 会话列表的最后一条消息预览：撤回/图片/文件用占位词，文本显示正文。
   * @param p 设备与会话合并后的条目（含 last/lastKind/lastRecalled）
   * @returns string 当前语言下的预览文案
   */
  const preview = (p: { last: string; lastKind: string; lastRecalled: boolean }) =>
    p.lastRecalled
      ? t('fst.chat.recalled')
      : p.lastKind === 'image'
        ? t('fst.chat.previewImage')
        : p.lastKind === 'file'
          ? t('fst.chat.previewFile')
          : p.last
  /**
   * 取附件对应的活动传输记录；传输完成后 store 会移除，因此可能为 undefined。
   * @param id 传输记录 ID
   * @returns Fst.Transfer | undefined 活动中的传输记录
   */
  const attachTransfer = (id?: string) => (id ? fst.transfers[id] : undefined)
  /**
   * 文件卡片标题：活动传输显示真实文件名，否则回退通用「文件」文案。
   * @param m 消息
   * @returns string 卡片标题
   */
  const fileTitle = (m: Fst.Message) => attachTransfer(m.attach[0])?.file_name || t('fst.chat.fileCard')
  /**
   * 文件卡片副标题：大小与百分比；没有活动传输时返回空串。
   * @param m 消息
   * @returns string 例如 "8.0 MB · 42%"
   */
  const fileMeta = (m: Fst.Message) => {
    const tr = attachTransfer(m.attach[0])
    if (!tr) return ''
    const pct = tr.size ? Math.round((tr.bytes_done / tr.size) * 100) : 0
    return `${formatBytes(tr.size)} · ${pct}%`
  }
  /**
   * 文件卡片进度条百分比；没有活动传输时返回 null（模板隐藏进度条）。
   * @param m 消息
   * @returns number | null 0-100 的整数
   */
  const filePercent = (m: Fst.Message) => {
    const tr = attachTransfer(m.attach[0])
    if (!tr) return null
    return tr.size ? Math.min(100, Math.round((tr.bytes_done / tr.size) * 100)) : 0
  }
  /**
   * 判断是否展示撤回入口：自己发出的、未撤回、且仍在 2 分钟窗口内。
   * @param m 消息
   * @returns boolean 可撤回为 true
   */
  const canRecall = (m: Fst.Message) =>
    m.direction === 'out' && !m.recalled && m.status !== 'sending' && Date.now() - m.ts <= RECALL_WINDOW
  /**
   * 判断是否展示“重试”入口：自己发出的待发送/失败消息，且未撤回。
   * @param m 消息
   * @returns boolean 可重试为 true
   */
  const canRetry = (m: Fst.Message) =>
    m.direction === 'out' && !m.recalled && (m.status === 'queued' || m.status === 'failed')
  /**
   * 撤回按钮置灰的原因文案。
   * @param m 消息
   * @returns string 超过 2 分钟时返回提示；其余情况为空串
   */
  const recallReason = (m: Fst.Message) =>
    m.direction === 'out' && !m.recalled && Date.now() - m.ts > RECALL_WINDOW
      ? t('fst.chat.recallExpired')
      : ''
  /**
   * 写剪贴板；WebView 不提供 navigator.clipboard 时回退到隐藏 textarea。
   * @param value 要复制的纯文本
   * @returns Promise<boolean> 是否复制成功
   */
  const copyText = async (value: string) => {
    try {
      await navigator.clipboard.writeText(value)
      return true
    } catch {
      const el = document.createElement('textarea')
      el.value = value
      el.style.position = 'fixed'
      el.style.opacity = '0'
      document.body.appendChild(el)
      el.select()
      const ok = document.execCommand('copy')
      document.body.removeChild(el)
      return ok
    }
  }
  /**
   * 把一条文本消息的纯文本复制到剪贴板。
   * @param m 消息（富文本会去掉标签）
   * @returns Promise<void> 无返回；失败时提示
   */
  const copyMessage = async (m: Fst.Message) => {
    const plain = m.content.replace(/<[^>]*>/g, '').replace(/&nbsp;/g, ' ').trim()
    if (await copyText(plain)) ElMessage.success(t('fst.chat.copied'))
    else ElMessage.warning(t('fst.chat.copyFailed'))
  }
  /**
   * 开始引用一条消息：输入区展示引用块，发送时随正文提交。
   * @param m 被引用的消息
   * @returns void
   */
  const startQuote = (m: Fst.Message) => {
    quoting.value = m
    editorRef.value?.focus()
  }

  /**
   * 切换私聊/群聊模式。
   * @param next 目标模式
   * @returns void
   */
  const switchMode = (next: 'private' | 'group') => {
    mode.value = next
    quoting.value = null
    if (next === 'group') {
      fst.loadRooms().catch(() => {})
      nextTick(() => groupBox.value && (groupBox.value.scrollTop = groupBox.value.scrollHeight))
    } else {
      toBottom()
    }
  }
  /**
   * 打开群聊房间并滚动到底部。
   * @param id 房间 ID
   * @returns Promise<void> 无返回
   */
  const selectRoom = async (id: string) => {
    mode.value = 'group'
    quoting.value = null
    await fst.openRoom(id)
    nextTick(() => groupBox.value && (groupBox.value.scrollTop = groupBox.value.scrollHeight))
  }
  /**
   * 房间类型标签文案。
   * @param r 房间（可能为空）
   * @returns string 局域网 / 分组
   */
  const roomKind = (r?: Fst.Room) => (r?.kind === 'lan' ? t('fst.chat.roomLAN') : t('fst.chat.roomGroup'))
  /**
   * 取群聊发送者头像（按 PeerID 查设备列表）。
   * @param id 发送者 PeerID
   * @returns string | undefined base64 头像
   */
  const peerAvatar = (id: string) => fst.peers.find((p) => p.peer_id === id)?.avatar
  /**
   * 发送一条群聊文本，成功后清空输入并滚到底部。
   * @returns Promise<void> 无返回
   */
  const sendGroup = async () => {
    const body = groupText.value.trim()
    if (!body || !fst.activeRoom || !activeRoomInfo.value?.joined) return
    groupText.value = ''
    await fst.sendRoomMessage(fst.activeRoom, body)
    nextTick(() => groupBox.value && (groupBox.value.scrollTop = groupBox.value.scrollHeight))
  }
  /**
   * 手动重投一条待发送/失败消息。
   * @param m 目标消息
   * @returns Promise<void> 无返回
   */
  const retry = async (m: Fst.Message) => {
    await fst.retryMessage(m.peer_id, m.id)
  }

  /** 传输状态变化时 +1：让未接收完的图片重新请求媒体端点，完成后自动显示 */
  const mediaTick = ref(0)
  watch(() => fst.transferTick, () => mediaTick.value++)

  const toBottom = () => nextTick(() => box.value && (box.value.scrollTop = box.value.scrollHeight))
  const select = async (id: string) => {
    more.value = true
    quoting.value = null
    await fst.openChat(id)
    toBottom()
  }
  const older = async () => {
    const h = box.value?.scrollHeight || 0
    more.value = (await fst.loadOlder(fst.activePeer)) > 0
    nextTick(() => box.value && (box.value.scrollTop = box.value.scrollHeight - h))
  }
  const send = async () => {
    if (!hasMessage.value) return
    const body = text.value
    const reply = quoting.value
      ? { id: quoting.value.id, preview: quotePreview.value, sender: quoteSender.value }
      : undefined
    text.value = ''
    quoting.value = null
    await fst.sendMessage(fst.activePeer, body, 'text', [], reply)
    toBottom()
  }
  /**
   * 选图片并作为图片气泡发送。
   * @returns Promise<void> 无返回；未选中图片时提示
   */
  const sendImages = async () => {
    const paths = await fetchPickFiles()
    const images = paths.filter((path) => /\.(png|jpe?g|gif|webp|bmp|ico)$/i.test(path))
    if (!images.length) {
      ElMessage.warning(t('fst.chat.noImages'))
      return
    }
    await sendAttachments(images, 'image')
  }
  /**
   * 选任意文件并作为文件卡片发送。
   * @returns Promise<void> 无返回；未选中文件时直接返回
   */
  const sendFiles = async () => {
    const paths = await fetchPickFiles()
    if (paths.length) await sendAttachments(paths, 'file')
  }
  /**
   * 先在文件通道建传输记录，再用返回的传输 ID 发一条附件消息（字节不经过前端）。
   * @param paths 本地文件路径列表
   * @param kind 气泡类型：image / file
   * @returns Promise<void> 无返回；记录创建失败时提示由 http 层处理
   */
  const sendAttachments = async (paths: string[], kind: 'image' | 'file') => {
    const r = await fetchSendFiles(fst.activePeer, paths)
    if (!r.ids?.length) return
    const attach = r.ids.slice(0, MAX_ATTACH)
    if (r.ids.length > attach.length) ElMessage.warning(t('fst.chat.attachLimit', { n: MAX_ATTACH }))
    await fst.sendMessage(fst.activePeer, '', kind, attach)
    toBottom()
  }
  /**
   * 撤回一条自己发送的消息（二次确认后交给 Go，对端同步撤回）。
   * @param m 目标消息
   * @returns Promise<void> 取消或失败时不改动本地状态
   */
  const recall = async (m: Fst.Message) => {
    try {
      await ElMessageBox.confirm(t('fst.chat.recallConfirm'), t('fst.chat.recall'), {
        type: 'warning'
      })
    } catch {
      return
    }
    await fst.recallMessage(m.peer_id, m.id)
    if (quoting.value?.id === m.id) quoting.value = null
  }
  /**
   * 在系统文件管理器中定位一条已完成传输。
   * @param id 传输记录 ID（文件卡片附件）
   * @returns Promise<void> 无返回；无 ID 时忽略
   */
  const revealTransfer = async (id?: string) => {
    if (id) await fetchTransferAction(id, 'reveal')
  }
  const handleCreated = (editor: IDomEditor) => {
    editorRef.value = editor
  }

  watch(() => msgs.value.length, toBottom)
  watch(
    () => groupMsgs.value.length,
    () => nextTick(() => groupBox.value && (groupBox.value.scrollTop = groupBox.value.scrollHeight))
  )
  watch(
    () => route.query.peer,
    (id) => {
      if (!id || route.name !== 'Chat') return
      mode.value = 'private'
      select(String(id))
    },
    { immediate: true }
  )
  onActivated(() => fst.activePeer && fst.readChat(fst.activePeer))
  onBeforeUnmount(() => editorRef.value?.destroy())
</script>

<style scoped>
  .chat-editor :deep(.w-e-toolbar) {
    border-bottom: 1px solid var(--el-border-color-lighter);
  }

  .chat-editor :deep(.w-e-text-container) {
    min-height: 72px;
    max-height: 120px;
  }

  .message-content :deep(p) {
    margin: 0;
  }

  .message-content :deep(p + p) {
    margin-top: 0.35rem;
  }

  .message-content :deep(ul),
  .message-content :deep(ol) {
    padding-left: 1.25rem;
  }

  .message-content :deep(a) {
    text-decoration: underline;
  }
</style>
