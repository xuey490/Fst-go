<!-- 设备页：在线终端 / 中继服务器 / 离线终端；拖文件到卡片即发送 -->
<template>
  <div class="p-4">
    <ElCard shadow="never" class="mb-4">
      <div class="flex items-center justify-between flex-wrap gap-3">
        <div>
          <div class="text-lg font-medium">{{ fst.self?.name || t('fst.device.self') }}</div>
          <div class="text-sm text-g-600 mt-1 break-all">
            {{ t('fst.device.peerId') }}：{{ fst.self?.peer_id }}
            <ElLink type="primary" class="ml-2" @click="copy(fst.self?.peer_id)">{{
              t('fst.common.copy')
            }}</ElLink>
          </div>
        </div>
        <div class="flex items-center gap-3">
          <ElButton type="primary" @click="openAdd">{{ t('fst.device.addDevice') }}</ElButton>
        </div>
      </div>
    </ElCard>

    <ElTabs v-model="activeTab" class="mb-4">
      <ElTabPane name="online" :label="t('fst.device.tabOnline')" />
      <ElTabPane name="relay" :label="t('fst.device.tabRelay')" />
      <ElTabPane name="offline" :label="t('fst.device.tabOffline')" />
    </ElTabs>

    <ElEmpty v-if="!list.length" :description="emptyDescription" />

    <div class="grid gap-4" style="grid-template-columns: repeat(auto-fill, minmax(260px, 1fr))">
      <ElCard
        v-for="p in list"
        :id="'peer-' + p.peer_id"
        :key="p.peer_id"
        :data-file-drop-target="p.online ? '' : undefined"
        shadow="hover"
        class="relative"
        :class="{ 'opacity-60': !p.online }"
      >
        <ElButton
          v-if="activeTab === 'offline'"
          text
          type="danger"
          size="small"
          class="absolute right-2 top-2 !p-1"
          :title="t('fst.device.removeDevice')"
          @click.stop="removeOffline(p)"
        >
          <ArtSvgIcon icon="ri:close-line" class="text-base" />
        </ElButton>
        <div class="flex items-center gap-3">
          <ElBadge is-dot :type="p.online ? 'success' : 'info'">
            <ElAvatar :size="44" :src="avatarURL(p.avatar)" class="!bg-g-200">
              <ArtSvgIcon icon="ri:computer-line" class="text-xl text-g-600" />
            </ElAvatar>
          </ElBadge>
          <div class="min-w-0 flex-1">
            <div class="font-medium truncate">{{ p.name || p.peer_id.slice(-8) }}</div>
            <div class="text-xs text-g-600 truncate">
              {{ p.platform }} ·
              {{
                p.online
                  ? p.ip || t('fst.device.online')
                  : t('fst.device.offlineSince', { time: ago(p.last_seen) })
              }}
              <ElTooltip
                v-if="p.online && !p.is_relay"
                :content="p.relayed ? t('fst.device.relayTip') : t('fst.device.directTip')"
              >
                <ElTag size="small" :type="p.relayed ? 'warning' : 'success'" class="ml-1">{{
                  p.relayed ? t('fst.device.relay') : t('fst.device.direct')
                }}</ElTag>
              </ElTooltip>
            </div>
          </div>
          <ElTooltip :content="t('fst.device.trustTip')">
            <ElCheckbox :model-value="p.trusted" @change="(v: any) => trust(p, !!v)">{{
              t('fst.device.trust')
            }}</ElCheckbox>
          </ElTooltip>
        </div>
        <div class="mt-4">
          <ElDropdown trigger="click" @command="(command) => handleAction(p, command)">
            <ElButton type="primary">
              {{ t('fst.device.operations') }}
              <ArtSvgIcon icon="ri:arrow-down-s-line" class="ml-1" />
            </ElButton>
            <template #dropdown>
              <ElDropdownMenu>
                <ElDropdownItem command="send" :disabled="!p.online">{{
                  t('fst.device.sendFile')
                }}</ElDropdownItem>
                <ElDropdownItem command="send-folder" :disabled="!p.online">{{
                  t('fst.device.sendFolder')
                }}</ElDropdownItem>
                <ElDropdownItem command="chat">{{ t('fst.device.chat') }}</ElDropdownItem>
              </ElDropdownMenu>
            </template>
          </ElDropdown>
        </div>
        <div v-if="peerTransfers(p.peer_id).length" class="mt-3 space-y-2">
          <div
            v-for="transfer in peerTransfers(p.peer_id)"
            :key="transfer.id"
            class="rounded bg-g-100 px-2 py-1.5"
          >
            <div class="flex items-center justify-between gap-2 text-xs text-g-600">
              <span class="truncate" :title="transfer.file_name">{{ transfer.file_name }}</span>
              <span class="shrink-0">{{ transferDirection(transfer) }}</span>
            </div>
            <ElProgress
              :percentage="transferPercent(transfer)"
              :stroke-width="6"
              :show-text="false"
              class="mt-1"
            />
            <div class="text-xs text-g-500">
              {{ formatBytes(transfer.bytes_done) }} / {{ formatBytes(transfer.size) }}
              <span v-if="transferSpeed(transfer)"> · {{ transferSpeed(transfer) }}</span>
            </div>
          </div>
        </div>
        <div v-else-if="p.online" class="text-xs text-g-500 mt-2">
          {{ t('fst.device.dropHint') }}
        </div>
      </ElCard>
    </div>

    <ElDialog v-model="addOpen" :title="t('fst.device.addTitle')" width="520px">
      <ElRadioGroup v-model="mode" class="mb-3">
        <ElRadioButton value="addr">{{ t('fst.device.modeAddr') }}</ElRadioButton>
        <ElRadioButton v-if="fst.features.wan_unlocked" value="relay">{{
          t('fst.device.modeRelay')
        }}</ElRadioButton>
        <ElRadioButton v-if="fst.features.wan_unlocked" value="id">{{
          t('fst.device.modeId')
        }}</ElRadioButton>
      </ElRadioGroup>
      <ElInput
        v-if="mode === 'addr'"
        v-model="addr"
        :placeholder="t('fst.device.addrPlaceholder')"
      />
      <template v-else>
        <ElInput
          v-model="peerId"
          :placeholder="t('fst.device.peerPlaceholder')"
          class="mb-2"
        />
        <div v-if="mode === 'id'" class="text-xs text-g-600">{{ t('fst.device.idHint') }}</div>
        <ElInput v-else v-model="relay" :placeholder="t('fst.device.relayPlaceholder')" />
      </template>
      <template #footer>
        <ElButton @click="addOpen = false">{{ t('fst.common.cancel') }}</ElButton>
        <ElButton type="primary" :loading="adding" :disabled="!target" @click="add">{{
          t('fst.common.connect')
        }}</ElButton>
      </template>
    </ElDialog>
  </div>
</template>

<script setup lang="ts">
  import {
    fetchConnect,
    fetchDeletePeer,
    fetchPickFiles,
    fetchPickFolder,
    fetchSendFiles,
    fetchSetTrusted
  } from '@/api/fst'
  import { useFstStore } from '@/store/modules/fst'
  import { avatarURL } from '@/utils/fst'
  import { ElMessage, ElMessageBox } from 'element-plus'
  import { useI18n } from 'vue-i18n'

  defineOptions({ name: 'Device' })
  const fst = useFstStore()
  const router = useRouter()
  const { t } = useI18n()
  const activeTab = ref<'online' | 'relay' | 'offline'>('online')

  // ---- 添加设备：完整 multiaddr，或 PeerID + 中继地址（拼成 <relay>/p2p-circuit/p2p/<id>）----
  const addOpen = ref(false)
  const adding = ref(false)
  const mode = ref<'addr' | 'relay' | 'id'>('addr')
  const addr = ref('')
  const peerId = ref('')
  const relay = ref('')
  const target = computed(() =>
    mode.value === 'addr'
      ? addr.value.trim()
      : mode.value === 'id'
        ? peerId.value.trim()
        : peerId.value.trim() && relay.value.trim()
          ? `${relay.value.trim().replace(/\/+$/, '')}/p2p-circuit/p2p/${peerId.value.trim()}`
          : ''
  )
  /**
   * 打开添加设备对话框；高级网络功能锁定态只保留完整地址方式。
   * @returns void 无返回
   */
  const openAdd = () => {
    if (!fst.features.wan_unlocked) mode.value = 'addr'
    addOpen.value = true
  }
  const add = async () => {
    adding.value = true
    try {
      await fetchConnect(target.value)
      ElMessage.success(t('fst.device.connecting'))
      addOpen.value = false
      addr.value = peerId.value = relay.value = ''
    } finally {
      adding.value = false
    }
  }
  const list = computed(() =>
    fst.peers.filter((p) => {
      if (activeTab.value === 'offline') return !p.online && !p.is_relay
      if (activeTab.value === 'relay') return fst.features.wan_unlocked && p.is_relay
      return p.online && !p.is_relay
    })
  )
  /** 当前标签页的空状态文案（随语言切换） */
  const emptyDescription = computed(() => {
    if (activeTab.value === 'offline') return t('fst.device.emptyOffline')
    if (activeTab.value === 'relay') return t('fst.device.emptyRelay')
    return t('fst.device.emptyOnline')
  })
  const activeTransfers = computed(() =>
    Object.values(fst.transfers).filter((t) => ['pending', 'accepted', 'running'].includes(t.status))
  )
  const peerTransfers = (peerId: string) =>
    activeTransfers.value.filter((transfer) => transfer.peer_id === peerId)

  /**
   * 复制文本到剪贴板并提示。
   * @param s 待复制文本；为空时忽略
   * @returns Promise<void> 无返回
   */
  const copy = async (s?: string) => {
    if (!s) return
    await navigator.clipboard.writeText(s)
    ElMessage.success(t('fst.common.copied'))
  }
  /**
   * 把时间戳转成「多久以前」的文案。
   * @param ts 毫秒时间戳
   * @returns 当前语言下的相对时间
   */
  const ago = (ts: number) => {
    const m = Math.max(0, Math.floor((Date.now() - ts) / 60000))
    if (m < 1) return t('fst.device.justNow')
    if (m < 60) return t('fst.device.minutesAgo', { n: m })
    if (m < 1440) return t('fst.device.hoursAgo', { n: Math.floor(m / 60) })
    return t('fst.device.daysAgo', { n: Math.floor(m / 1440) })
  }
  /**
   * 选择文件并发起传输（目录会被 Go 递归展开）。
   * @param p 目标设备
   * @param folder true 表示选择整个文件夹
   * @returns Promise<void> 无返回；用户取消时不发请求
   */
  const send = async (p: Fst.Peer, folder = false) => {
    const paths = folder ? [await fetchPickFolder()] : await fetchPickFiles()
    if (!paths.length || !paths[0]) return
    const res = await fetchSendFiles(p.peer_id, paths)
    if (res.skipped?.length) {
      ElMessage.warning(t('fst.device.skippedHint', { n: res.skipped.length }))
    } else if (res.batch_ids?.length > 1) {
      ElMessage.success(t('fst.device.batchesSent', { n: res.batch_ids.length, files: res.files }))
    } else {
      ElMessage.success(t('fst.device.requestSent'))
    }
  }
  /**
   * 设备卡片「操作」菜单分发。
   * @param p 目标设备
   * @param command 菜单项：发送文件 / 发送文件夹 / 聊天
   * @returns Promise<void> 无返回
   */
  const handleAction = (p: Fst.Peer, command: 'send' | 'send-folder' | 'chat') => {
    if (command === 'send') return send(p)
    if (command === 'send-folder') return send(p, true)
    return chat(p)
  }
  const transferPercent = (transfer: Fst.Transfer) => {
    const current = fst.progress[transfer.id]?.bytes_done ?? transfer.bytes_done
    return transfer.size ? Math.min(100, Math.floor((current / transfer.size) * 100)) : 0
  }
  const formatBytes = (bytes: number) => {
    const units = ['B', 'KB', 'MB', 'GB', 'TB']
    let value = bytes
    let index = 0
    while (value >= 1024 && index < units.length - 1) {
      value /= 1024
      index++
    }
    return `${value.toFixed(index ? 1 : 0)} ${units[index]}`
  }
  const transferSpeed = (transfer: Fst.Transfer) => {
    const speed = fst.progress[transfer.id]?.speed
    return speed ? `${formatBytes(speed)}/s` : ''
  }
  /**
   * 取传输方向文案。
   * @param transfer 传输记录
   * @returns 当前语言下的方向文案
   */
  const transferDirection = (transfer: Fst.Transfer) =>
    transfer.direction === 'out' ? t('fst.transfer.outgoing') : t('fst.transfer.incoming')
  const trust = async (p: Fst.Peer, v: boolean) => {
    await fetchSetTrusted(p.peer_id, v)
  }
  const removeOffline = async (p: Fst.Peer) => {
    try {
      await ElMessageBox.confirm(
        t('fst.device.removeConfirm', { name: p.name || p.peer_id.slice(-8) }),
        t('fst.device.removeTitle'),
        {
          type: 'warning',
          confirmButtonText: t('fst.common.remove'),
          cancelButtonText: t('fst.common.cancel'),
          closeOnClickModal: false
        }
      )
      await fetchDeletePeer(p.peer_id)
      fst.removePeer(p.peer_id)
      ElMessage.success(t('fst.device.removed'))
    } catch (error) {
      if (error !== 'cancel' && error !== 'close') throw error
    }
  }
  const chat = (p: Fst.Peer) => router.push({ name: 'Chat', query: { peer: p.peer_id } })
</script>
