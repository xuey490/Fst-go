<!-- 消息悬停操作条：引用 / 复制 / 撤回；撤回超出时间窗时置灰并提示原因 -->
<template>
  <div
    class="absolute -top-3 right-0 z-10 flex items-center gap-0.5 rounded-md border border-g-300 bg-white px-1 py-0.5 shadow-sm opacity-0 pointer-events-none transition-opacity group-hover:opacity-100 group-hover:pointer-events-auto dark:bg-[#2b2b2b]"
  >
    <button
      type="button"
      class="px-1.5 py-0.5 text-xs text-g-600 rounded hover:text-theme hover:bg-g-200 cursor-pointer"
      @click="$emit('quote')"
    >
      {{ t('fst.chat.quote') }}
    </button>
    <button
      v-if="canCopy"
      type="button"
      class="px-1.5 py-0.5 text-xs text-g-600 rounded hover:text-theme hover:bg-g-200 cursor-pointer"
      @click="$emit('copy')"
    >
      {{ t('fst.chat.copy') }}
    </button>
    <button
      type="button"
      :disabled="!canRecall"
      :title="canRecall ? '' : recallReason"
      class="px-1.5 py-0.5 text-xs rounded"
      :class="
        canRecall
          ? 'text-g-600 hover:text-theme hover:bg-g-200 cursor-pointer'
          : 'text-g-400 cursor-not-allowed'
      "
      @click="canRecall && $emit('recall')"
    >
      {{ t('fst.chat.recall') }}
    </button>
  </div>
</template>

<script setup lang="ts">
  import { useI18n } from 'vue-i18n'

  defineOptions({ name: 'FstMessageActions' })

  defineProps<{
    /** 是否可以复制（仅未撤回的文本消息） */
    canCopy: boolean
    /** 是否可以撤回（自己发出、未撤回、未超过 2 分钟） */
    canRecall: boolean
    /** 撤回按钮置灰时的原因文案（如「超过 2 分钟，无法撤回」） */
    recallReason?: string
  }>()

  defineEmits<{
    /** 点击「引用」，父组件记录被引用消息 */
    quote: []
    /** 点击「复制」，父组件写剪贴板 */
    copy: []
    /** 点击「撤回」，父组件弹确认框并调用后端 */
    recall: []
  }>()

  const { t } = useI18n()
</script>
