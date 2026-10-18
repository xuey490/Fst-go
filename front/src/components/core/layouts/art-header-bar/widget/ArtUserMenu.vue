<!-- 本机菜单：昵称/PeerID、下载目录、隐藏到托盘、退出 -->
<template>
  <ElPopover
    placement="bottom-end"
    :width="260"
    :hide-after="0"
    :offset="10"
    trigger="hover"
    :show-arrow="false"
    popper-class="user-menu-popover"
    popper-style="padding: 5px 16px;"
  >
    <template #reference>
      <ArtSvgIcon icon="ri:computer-line" class="mr-5 text-2xl c-p" />
    </template>
    <template #default>
      <div class="pt-3">
        <div class="pb-1">
          <span class="block text-sm font-medium text-g-800 truncate">{{ fst.self?.name }}</span>
          <span class="block mt-0.5 text-xs text-g-500 truncate" :title="fst.self?.peer_id">
            {{ fst.self?.peer_id }}
          </span>
        </div>
        <ul class="py-4 mt-3 border-t border-g-300/80">
          <li class="btn-item" @click="copyId">
            <ArtSvgIcon icon="ri:file-copy-line" />
            <span>复制 PeerID</span>
          </li>
          <li class="btn-item" @click="fetchOpenDownloadDir()">
            <ArtSvgIcon icon="ri:folder-open-line" />
            <span>打开下载目录</span>
          </li>
          <li class="btn-item" @click="fetchHideWindow()">
            <ArtSvgIcon icon="ri:eye-off-line" />
            <span>隐藏到托盘</span>
          </li>
          <div class="w-full h-px my-2 bg-g-300/80"></div>
          <div class="log-out c-p" @click="quit">退出 FST</div>
        </ul>
      </div>
    </template>
  </ElPopover>
</template>

<script setup lang="ts">
  import { ElMessageBox, ElMessage } from 'element-plus'
  import { fetchHideWindow, fetchOpenDownloadDir, fetchQuit } from '@/api/fst'
  import { useFstStore } from '@/store/modules/fst'

  defineOptions({ name: 'ArtUserMenu' })
  const fst = useFstStore()

  const copyId = async () => {
    await navigator.clipboard.writeText(fst.self?.peer_id || '')
    ElMessage.success('已复制')
  }
  const quit = () =>
    ElMessageBox.confirm('退出后将无法接收文件和消息，确定退出？', '退出 FST', { type: 'warning' })
      .then(() => fetchQuit())
      .catch(() => {})
</script>
<style scoped>
  @reference '@styles/core/tailwind.css';

  @layer components {
    .btn-item {
      @apply flex items-center p-2 mb-3 select-none rounded-md cursor-pointer last:mb-0;

      span {
        @apply text-sm;
      }

      .art-svg-icon {
        @apply mr-2 text-base;
      }

      &:hover {
        background-color: var(--art-gray-200);
      }
    }
  }

  .log-out {
    @apply py-1.5
    mt-5
    text-xs
    text-center
    border
    border-g-400
    rounded-md
    transition-all
    duration-200
    hover:shadow-xl;
  }
</style>
