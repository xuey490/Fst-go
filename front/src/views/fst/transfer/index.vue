<!-- 传输页：历史 + 实时进度（useTable 分页 + Wails 进度事件） -->
<template>
  <div class="p-4 h-full flex flex-col gap-3">
    <ElCard shadow="never" body-class="!pb-0">
      <ElForm inline :model="searchParams">
        <ElFormItem :label="t('fst.transfer.fileName')">
          <ElInput
            v-model="searchParams.keyword"
            clearable
            :placeholder="t('fst.transfer.keyword')"
            @keyup.enter="search"
          />
        </ElFormItem>
        <ElFormItem :label="t('fst.transfer.direction')">
          <ElSelect v-model="searchParams.direction" clearable class="!w-28" @change="search">
            <ElOption :label="t('fst.transfer.out')" value="out" />
            <ElOption :label="t('fst.transfer.in')" value="in" />
          </ElSelect>
        </ElFormItem>
        <ElFormItem :label="t('fst.transfer.status')">
          <ElSelect v-model="searchParams.status" clearable class="!w-28" @change="search">
            <ElOption v-for="(key, k) in statusKey" :key="k" :label="t(key)" :value="k" />
          </ElSelect>
        </ElFormItem>
        <ElFormItem>
          <ElButton type="primary" @click="search">{{ t('fst.common.search') }}</ElButton>
          <ElButton @click="resetSearchParams">{{ t('fst.common.reset') }}</ElButton>
        </ElFormItem>
      </ElForm>
    </ElCard>

    <ElCard shadow="never" class="flex-1 min-h-0" body-class="h-full flex flex-col">
      <div class="flex items-center gap-2 pb-2 shrink-0">
        <ElButton type="danger" plain :disabled="!selected.length" @click="clearLogs">
          {{ t('fst.transfer.clear') }}
        </ElButton>
        <span class="text-xs text-g-600">
          {{
            selected.length
              ? t('fst.transfer.clearSelected', { n: selected.length })
              : t('fst.transfer.clearNone')
          }}
        </span>
      </div>
      <div class="flex-1 min-h-0">
        <ArtTable
          :loading="loading"
          :data="data"
          :columns="columns"
          row-key="id"
          :pagination="pagination"
          @selection-change="onSelect"
          @pagination:size-change="handleSizeChange"
          @pagination:current-change="handleCurrentChange"
        >
        <template #peer_id="{ row }">{{ fst.peerName(row.peer_id) }}</template>
        <template #direction="{ row }">
          <ElTag :type="row.direction === 'out' ? 'primary' : 'success'" size="small">
            {{ row.direction === 'out' ? t('fst.transfer.out') : t('fst.transfer.in') }}
          </ElTag>
        </template>
        <template #rel_path="{ row }">{{ row.rel_path || t('fst.common.dash') }}</template>
        <template #size="{ row }">{{ fmtSize(row.size) }}</template>
        <template #status="{ row }">
          <template v-if="row.status === 'running' || row.status === 'accepted'">
            <ElProgress :percentage="percent(row)" :stroke-width="10" />
            <div class="text-xs text-g-600">{{ speed(row) }}</div>
          </template>
          <ElTag v-else :type="tagType[row.status as Fst.TransferStatus]" size="small">
            {{ statusText(row.status as Fst.TransferStatus) }}
          </ElTag>
          <div v-if="row.error" class="text-xs text-red-500 truncate" :title="row.error">{{ row.error }}</div>
        </template>
        <template #created_at="{ row }">{{ new Date(row.created_at).toLocaleString() }}</template>
        <template #actions="{ row }">
          <ElLink v-if="active(row)" type="danger" @click="act(row, 'cancel')">{{ t('fst.transfer.cancel') }}</ElLink>
          <ElLink v-if="row.direction === 'out' && ['failed', 'cancelled', 'rejected'].includes(row.status)" type="primary" @click="act(row, 'retry')">{{ t('fst.transfer.retry') }}</ElLink>
          <ElLink v-if="row.direction === 'out' && ['failed', 'cancelled', 'rejected'].includes(row.status)" type="warning" @click="retryBatch(row)">{{ t('fst.transfer.retryBatch') }}</ElLink>
          <ElLink v-if="row.status === 'done' && row.direction === 'in'" type="primary" @click="act(row, 'reveal')">{{ t('fst.transfer.reveal') }}</ElLink>
        </template>
        </ArtTable>
      </div>
    </ElCard>
  </div>
</template>

<script setup lang="ts">
  import { useTable } from '@/hooks/core/useTable'
  import {
    fetchClearTransfers,
    fetchRetryBatch,
    fetchTransfers,
    fetchTransferAction
  } from '@/api/fst'
  import { useFstStore } from '@/store/modules/fst'
  import { ElMessage, ElMessageBox } from 'element-plus'
  import { useI18n } from 'vue-i18n'

  defineOptions({ name: 'Transfer' })
  const fst = useFstStore()
  const { t } = useI18n()

  /** 传输状态 -> i18n 词条键（表格与筛选下拉共用） */
  const statusKey: Record<Fst.TransferStatus, string> = {
    pending: 'fst.transfer.statusPending',
    accepted: 'fst.transfer.statusAccepted',
    running: 'fst.transfer.statusRunning',
    done: 'fst.transfer.statusDone',
    failed: 'fst.transfer.statusFailed',
    cancelled: 'fst.transfer.statusCancelled',
    rejected: 'fst.transfer.statusRejected'
  }
  /**
   * 取传输状态的中文/英文文案。
   * @param s 传输状态
   * @returns 当前语言下的状态文案
   */
  const statusText = (s: Fst.TransferStatus) => t(statusKey[s])
  const tagType: Record<Fst.TransferStatus, 'info' | 'success' | 'danger' | 'warning' | 'primary'> = {
    pending: 'warning',
    accepted: 'primary',
    running: 'primary',
    done: 'success',
    failed: 'danger',
    cancelled: 'info',
    rejected: 'info'
  }

  /** 是否仍在传输中（运行中的记录不允许勾选清理） */
  const active = (r: Fst.Transfer) => ['pending', 'accepted', 'running'].includes(r.status)

  const { data, loading, pagination, searchParams, columns, handleSizeChange, handleCurrentChange, resetSearchParams, refreshSoft, getData } =
    useTable({
      core: {
        apiFn: fetchTransfers,
        apiParams: { page: 1, limit: 20, keyword: '', direction: '', status: '' },
        paginationKey: { current: 'page', size: 'limit' },
        columnsFactory: () => [
          { type: 'selection', width: 48, fixed: 'left', selectable: (row: Fst.Transfer) => !active(row) },
          { prop: 'file_name', label: t('fst.transfer.fileName'), minWidth: 200, showOverflowTooltip: true },
          { prop: 'rel_path', label: t('fst.transfer.folder'), minWidth: 180, showOverflowTooltip: true, useSlot: true },
          { prop: 'peer_id', label: t('fst.transfer.device'), width: 140, useSlot: true },
          { prop: 'direction', label: t('fst.transfer.direction'), width: 80, useSlot: true },
          { prop: 'size', label: t('fst.transfer.size'), width: 100, useSlot: true },
          { prop: 'status', label: t('fst.transfer.status'), minWidth: 180, useSlot: true },
          { prop: 'created_at', label: t('fst.transfer.time'), width: 170, useSlot: true },
          { prop: 'actions', label: t('fst.transfer.actions'), width: 210, fixed: 'right', useSlot: true }
        ]
      },
      // 数据随传输实时变化，禁用缓存
      performance: { enableCache: false }
    })

  const search = () => getData()
  const fmtSize = (n: number) => {
    const u = ['B', 'KB', 'MB', 'GB', 'TB']
    let i = 0
    while (n >= 1024 && i < u.length - 1) (n /= 1024), i++
    return `${n.toFixed(i ? 1 : 0)} ${u[i]}`
  }
  const percent = (r: Fst.Transfer) => {
    const p = fst.progress[r.id]
    const done = p ? p.bytes_done : r.bytes_done
    return r.size ? Math.min(100, Math.floor((done / r.size) * 100)) : 0
  }
  const speed = (r: Fst.Transfer) => (fst.progress[r.id] ? fmtSize(fst.progress[r.id].speed) + '/s' : '')
  const act = async (r: Fst.Transfer, a: 'cancel' | 'retry' | 'reveal') => {
    await fetchTransferAction(r.id, a)
    if (a !== 'reveal') refreshSoft()
  }
  /**
   * 继续整批：只重发该批次里仍未完成的文件。
   * @param r 该批次任意一条出站记录
   * @returns Promise<void> 无返回；成功后刷新当前页
   */
  const retryBatch = async (r: Fst.Transfer) => {
    await fetchRetryBatch(r.batch_id)
    ElMessage.success(t('fst.transfer.batchRetried'))
    refreshSoft()
  }

  /** 当前勾选的传输记录（只允许勾选已结束的） */
  const selected = ref<Fst.Transfer[]>([])
  /**
   * 同步表格勾选结果。
   * @param rows 当前选中的行
   * @returns 无
   */
  const onSelect = (rows: Fst.Transfer[]) => {
    selected.value = rows
  }
  /**
   * 清理传输日志：只删除当前勾选的记录，未勾选的保持不动。
   * 磁盘文件与聊天引用的记录会保留。
   * @returns Promise<void> 无返回；未勾选或用户取消时不发请求
   */
  const clearLogs = async () => {
    const ids = selected.value.map((r) => r.id)
    if (!ids.length) return
    try {
      await ElMessageBox.confirm(t('fst.transfer.clearSelectedConfirm', { n: ids.length }), t('fst.transfer.clearTitle'), {
        type: 'warning'
      })
    } catch {
      return
    }
    const r = await fetchClearTransfers(ids)
    selected.value = []
    if (ids.length && r.deleted < ids.length) ElMessage.warning(t('fst.transfer.clearKept'))
    else ElMessage.success(t('fst.transfer.cleared', { n: r.deleted }))
    refreshSoft()
  }

  // 状态变化（新建/完成/失败…）时刷新当前页；进度本身只改本地 progress，不请求
  let timer = 0
  watch(
    () => fst.transferTick,
    () => {
      clearTimeout(timer)
      timer = window.setTimeout(refreshSoft, 300)
    }
  )
  onUnmounted(() => clearTimeout(timer))
</script>