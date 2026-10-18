<!-- 设置页：昵称、下载目录、并发、自动接收、开机自启、本机地址与手动连接 -->
<template>
  <div class="p-4 max-w-3xl">
    <ElCard shadow="never" class="mb-4">
      <template #header>
        <span class="select-none" @click="tapTitle">{{ t('fst.settings.selfTitle') }}</span>
      </template>
      <ElForm label-width="110px">
        <ElFormItem :label="t('fst.settings.nickname')">
          <ElInput v-model="name" maxlength="32" class="!w-64" />
          <ElButton class="ml-2" type="primary" @click="saveName">{{
            t('fst.common.save')
          }}</ElButton>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.avatar')">
          <ElAvatar :size="48" :src="selfAvatar">
            <ArtSvgIcon icon="ri:user-line" />
          </ElAvatar>
          <ElButton class="ml-3" @click="pickAvatar">{{ t('fst.settings.avatarPick') }}</ElButton>
          <ElButton v-if="fst.self?.avatar" class="ml-1" @click="clearAvatar">{{
            t('fst.settings.avatarClear')
          }}</ElButton>
          <span class="text-xs text-g-600 ml-2">{{ t('fst.settings.avatarHint') }}</span>
        </ElFormItem>
        <ElFormItem label="PeerID">
          <span class="break-all">{{ fst.self?.peer_id }}</span>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.listenAddrs')">
          <div class="w-full">
            <div v-for="a in addrs" :key="a" class="flex items-start gap-2 text-sm">
              <span class="break-all">{{ a }}</span>
              <ElLink type="primary" class="shrink-0" @click="copy(a)">{{
                t('fst.common.copy')
              }}</ElLink>
            </div>
            <div class="text-xs text-g-600 mt-1">{{ t('fst.settings.addrHint') }}</div>
          </div>
        </ElFormItem>
      </ElForm>
    </ElCard>

    <ElCard shadow="never" :header="t('fst.settings.transferTitle')" class="mb-4" v-if="s">
      <ElForm label-width="110px">
        <ElFormItem :label="t('fst.settings.downloadDir')">
          <ElInput v-model="s.download_dir" class="!w-96" />
          <ElButton class="ml-2" @click="pick">{{ t('fst.common.pick') }}</ElButton>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.concurrency')">
          <ElInputNumber v-model="s.concurrency" :min="1" :max="8" />
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.rateLimit')">
          <ElInputNumber v-model="s.rate_limit_kbps" :min="0" :max="1048576" :step="256" />
          <span class="text-xs text-g-600 ml-2">{{ t('fst.settings.rateLimitHint') }}</span>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.prehash')">
          <ElSwitch v-model="s.prehash" />
          <span class="text-xs text-g-600 ml-2">{{ t('fst.settings.prehashHint') }}</span>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.autoAccept')">
          <ElSwitch v-model="s.auto_accept" />
          <span class="text-xs text-g-600 ml-2">{{ t('fst.settings.autoAcceptHint') }}</span>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.autostart')">
          <ElSwitch v-model="s.autostart" />
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.closeAction')">
          <ElRadioGroup v-model="s.close_action">
            <ElRadio value="ask">{{ t('fst.settings.closeAsk') }}</ElRadio>
            <ElRadio value="tray">{{ t('fst.settings.closeTray') }}</ElRadio>
            <ElRadio value="quit">{{ t('fst.settings.closeQuit') }}</ElRadio>
          </ElRadioGroup>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.singleInstance')">
          <ElSwitch v-model="s.single_instance" />
          <span class="text-xs text-g-600 ml-2">{{ t('fst.settings.singleInstanceHint') }}</span>
        </ElFormItem>
        <ElFormItem>
          <ElButton type="primary" @click="save">{{ t('fst.common.saveSettings') }}</ElButton>
        </ElFormItem>
      </ElForm>
    </ElCard>

    <ElCard
      shadow="never"
      :header="t('fst.settings.wanTitle')"
      class="mb-4"
      v-if="s && fst.features.wan_unlocked"
    >
      <ElForm label-width="110px">
        <ElFormItem :label="t('fst.settings.wanEnable')">
          <ElSwitch v-model="s.wan_enabled" />
          <span class="text-xs text-g-600 ml-2">{{ t('fst.settings.wanEnableHint') }}</span>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.reachability')" v-if="fst.self?.wan">
          <ElTag :type="reach.type">{{ reach.text }}</ElTag>
          <span class="text-xs text-g-600 ml-2">{{ t('fst.settings.reachHint') }}</span>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.relays')">
          <ElInput
            v-model="relaysText"
            type="textarea"
            :rows="3"
            class="!w-[28rem]"
            :placeholder="t('fst.settings.relaysPlaceholder')"
          />
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.group')">
          <ElInput
            v-model="s.group"
            class="!w-[28rem]"
            :placeholder="t('fst.settings.groupPlaceholder')"
            show-password
          />
          <span class="text-xs text-g-600 ml-2">{{ t('fst.settings.groupHint') }}</span>
        </ElFormItem>
        <ElFormItem
          :label="t('fst.settings.groupStatus')"
          v-if="s.relay_group_name || s.relay_group_error"
        >
          <ElTag v-if="s.relay_group_error" type="danger">{{ s.relay_group_error }}</ElTag>
          <template v-else>
            <ElTag type="success">{{ s.relay_group_name }}</ElTag>
            <span class="text-xs text-g-600 ml-2">
              {{
                s.relay_group_expires
                  ? t('fst.settings.groupExpires', {
                      time: new Date(s.relay_group_expires).toLocaleString()
                    })
                  : t('fst.settings.groupNever')
              }}
            </span>
          </template>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.configPath')">
          <span class="text-xs text-g-600 break-all">
            {{ s.config_path || t('fst.settings.configPathEmpty') }}
          </span>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.qrcode')" v-if="shareAddr">
          <div>
            <QrcodeVue :value="shareAddr" :size="160" level="M" />
            <div class="text-xs text-g-600 mt-1 break-all max-w-md">{{ shareAddr }}</div>
          </div>
        </ElFormItem>
        <ElFormItem>
          <ElButton type="primary" @click="save">{{ t('fst.common.saveSettings') }}</ElButton>
        </ElFormItem>
      </ElForm>
    </ElCard>

    <ElCard
      shadow="never"
      :header="t('fst.settings.proxyTitle')"
      class="mb-4"
      v-if="s && fst.features.wan_unlocked"
    >
      <ElAlert type="warning" :closable="false" class="mb-4" :title="t('fst.settings.proxyAlert')" />
      <ElForm label-width="110px">
        <ElFormItem :label="t('fst.settings.proxyStatus')">
          <template v-if="!fst.proxyStatus || !fst.proxyStatus.enabled">
            <ElTag type="info">{{ t('fst.settings.proxyDisabled') }}</ElTag>
            <span class="text-xs text-g-600 ml-2">{{ t('fst.settings.proxyDisabledHint') }}</span>
          </template>
          <template v-else-if="fst.proxyStatus.running">
            <ElTag type="success">{{ t('fst.settings.proxyRunning') }}</ElTag>
            <span v-if="fst.proxyStatus.probe === 'ok'" class="text-xs text-g-600 ml-2">{{
              t('fst.settings.proxyProbeOK')
            }}</span>
            <span v-else-if="fst.proxyStatus.probe" class="text-xs text-red-500 ml-2">{{
              t('fst.settings.proxyProbeFail', { err: fst.proxyStatus.probe })
            }}</span>
            <span v-else class="text-xs text-g-600 ml-2">{{ t('fst.settings.proxyProbing') }}</span>
          </template>
          <template v-else>
            <ElTag type="danger">{{ t('fst.settings.proxyNotRunning') }}</ElTag>
            <span class="text-xs text-red-500 ml-2 break-all">{{
              fst.proxyStatus.error || t('fst.settings.proxyUnknownReason')
            }}</span>
          </template>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.proxySocks')" v-if="fst.proxyStatus?.socks_addr">
          <span class="break-all">{{ fst.proxyStatus.socks_addr }}</span>
          <ElLink type="primary" class="shrink-0 ml-2" @click="copy(fst.proxyStatus!.socks_addr!)">{{
            t('fst.common.copy')
          }}</ElLink>
          <span class="text-xs text-g-600 ml-2">{{ t('fst.settings.proxySocksHint') }}</span>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.proxyEnable')">
          <ElSwitch v-model="s.proxy_enable" />
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.proxyMode')">
          <ElRadioGroup v-model="s.proxy_mode">
            <ElRadio value="split">{{ t('fst.settings.proxyModeSplit') }}</ElRadio>
            <ElRadio value="global">{{ t('fst.settings.proxyModeGlobal') }}</ElRadio>
          </ElRadioGroup>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.proxyExit')">
          <ElSelect
            v-model="s.proxy_exit"
            filterable
            class="!w-[28rem]"
            :placeholder="t('fst.settings.proxyExitPlaceholder')"
          >
            <ElOption v-for="r in s.relays" :key="r" :label="r" :value="r" />
          </ElSelect>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.proxyRoutes')" v-if="s.proxy_mode === 'split'">
          <ElInput
            v-model="proxyRoutesText"
            type="textarea"
            :rows="4"
            class="!w-[28rem]"
            :placeholder="t('fst.settings.proxyRoutesPlaceholder')"
          />
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.proxyDomains')" v-if="s.proxy_mode === 'split'">
          <ElInput
            v-model="proxyDomainsText"
            type="textarea"
            :rows="4"
            class="!w-[28rem]"
            :placeholder="t('fst.settings.proxyDomainsPlaceholder')"
          />
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.proxyToken')">
          <ElInput
            v-model="s.proxy_token"
            show-password
            class="!w-[28rem]"
            :placeholder="t('fst.settings.proxyTokenPlaceholder')"
          />
          <span class="text-xs text-g-600 ml-2">{{ t('fst.settings.restartHint') }}</span>
        </ElFormItem>
        <ElFormItem :label="t('fst.settings.proxyIPv6')">
          <ElSwitch v-model="s.proxy_ipv6" />
          <ElTag class="ml-2" type="info" size="small">{{ t('fst.settings.experimental') }}</ElTag>
          <span class="text-xs text-g-600 ml-2">{{ t('fst.settings.proxyIPv6Hint') }}</span>
        </ElFormItem>
        <ElDivider content-position="left">{{ t('fst.settings.socksTitle') }}</ElDivider>
        <ElFormItem :label="t('fst.settings.socksEnable')">
          <ElRadioGroup v-model="s.socks_enable">
            <ElRadio :value="true">{{ t('fst.settings.socksOn') }}</ElRadio>
            <ElRadio :value="false">{{ t('fst.settings.socksOff') }}</ElRadio>
          </ElRadioGroup>
          <span class="text-xs text-g-600 ml-2">{{ t('fst.settings.socksEnableHint') }}</span>
        </ElFormItem>
        <ElAlert
          v-if="s.socks_enable && !s.socks_user"
          type="warning"
          :closable="false"
          show-icon
          class="mb-3"
          :title="t('fst.settings.socksNoAuthWarn')"
        />
        <template v-if="s.socks_enable">
          <ElFormItem :label="t('fst.settings.socksUser')">
            <ElInput v-model="s.socks_user" class="!w-64" autocomplete="off" />
            <span class="text-xs text-g-600 ml-2">{{ t('fst.settings.socksAuthHint') }}</span>
          </ElFormItem>
          <ElFormItem :label="t('fst.settings.socksPass')">
            <ElInput v-model="s.socks_pass" show-password class="!w-64" autocomplete="new-password" />
          </ElFormItem>
          <ElFormItem :label="t('fst.settings.socksListen')">
            <ElInput
              v-model="s.socks_listen"
              class="!w-64"
              :placeholder="t('fst.settings.socksListenPlaceholder')"
            />
          </ElFormItem>
          <ElFormItem :label="t('fst.settings.socksAllow')">
            <ElInput
              v-model="socksAllowText"
              type="textarea"
              :rows="2"
              class="!w-[28rem]"
              :placeholder="t('fst.settings.socksAllowPlaceholder')"
            />
          </ElFormItem>
          <ElFormItem :label="t('fst.settings.socksLimits')">
            <ElInputNumber v-model="s.socks_max_conn" :min="0" :max="1024" />
            <span class="text-xs text-g-600 mx-2">{{ t('fst.settings.socksMaxTotal') }}</span>
            <ElInputNumber v-model="s.socks_max_per_ip" :min="0" :max="256" />
            <span class="text-xs text-g-600 mx-2">{{ t('fst.settings.socksMaxPerIP') }}</span>
          </ElFormItem>
        </template>
        <ElFormItem>
          <ElButton type="primary" @click="save">{{ t('fst.common.saveSettings') }}</ElButton>
        </ElFormItem>
      </ElForm>
    </ElCard>

    <ElCard shadow="never" :header="t('fst.settings.manualTitle')">
      <ElInput
        v-model="addr"
        :placeholder="t('fst.settings.manualPlaceholder')"
        class="!w-[28rem]"
      />
      <ElButton class="ml-2" type="primary" :disabled="!addr.trim()" @click="connect">{{
        t('fst.common.connect')
      }}</ElButton>
    </ElCard>
  </div>
</template>

<script setup lang="ts">
  import { ElMessage, ElMessageBox } from 'element-plus'
  import QrcodeVue from 'qrcode.vue'
  import {
    fetchConnect,
    fetchPickDir,
    fetchPickFiles,
    fetchSaveSettings,
    fetchSetAvatar,
    fetchSetName,
    fetchSettings
  } from '@/api/fst'
  import { useFstStore } from '@/store/modules/fst'
  import { useI18n } from 'vue-i18n'
  import { avatarURL } from '@/utils/fst'

  defineOptions({ name: 'Settings' })
  const fst = useFstStore()
  const { t } = useI18n()
  const s = ref<Fst.Settings>()
  const name = ref('')
  const addr = ref('')
  // 高级网络功能门禁：默认关闭，解锁入口见 docs/开发文档.md。
  let taps = 0
  let lastTapAt = 0
  /**
   * 按内置序列切换高级网络功能开关（界面不提供任何提示）。
   * @returns Promise<void> 无返回；锁定需二次确认，取消时静默返回
   */
  const tapTitle = async () => {
    const now = Date.now()
    taps = now - lastTapAt < 1500 ? taps + 1 : 1
    lastTapAt = now
    if (taps < 7) return
    taps = 0
    if (fst.features.wan_unlocked) {
      try {
        await ElMessageBox.confirm(t('fst.feature.lockConfirm'), t('fst.feature.title'), {
          type: 'warning'
        })
      } catch {
        return
      }
      if (await fst.setWanUnlocked(false)) {
        ElMessage.success(t('fst.feature.locked'))
        ElMessage.info(t('fst.feature.restart'))
      }
    } else if (await fst.setWanUnlocked(true)) {
      ElMessage.success(t('fst.feature.unlocked'))
      ElMessage.info(t('fst.feature.restart'))
    }
  }
  // 手动连接只需要 TCP 的局域网地址，回环地址对别的机器没用
  const addrs = computed(() =>
    (fst.self?.addrs || []).filter((a) => a.includes('/tcp/') && !/\/(ip4\/127\.|ip6\/::1\/)/.test(a))
  )

  // 多行文本框（中继地址 / 代理网段 / 代理域名）以纯文本编辑，保存时才解析成数组。
  // 之前用 computed 的 setter 即时 split(/\s+/)，导致刚敲下的换行/空格立刻被抹掉，
  // 永远无法输入第二条。
  const relaysText = ref('')
  const proxyRoutesText = ref('')
  const proxyDomainsText = ref('')
  /** SOCKS5 来源白名单以多行文本编辑，保存时才解析成 CIDR 数组 */
  const socksAllowText = ref('')
  // 换行、空格、逗号、分号都当分隔符，并去重
  const parseLines = (v: string) =>
    [...new Set(v.split(/[\s,;]+/).map((x) => x.trim()).filter(Boolean))]
  const loadText = (cfg: Fst.Settings) => {
    relaysText.value = (cfg.relays || []).join('\n')
    proxyRoutesText.value = (cfg.proxy_routes || []).join('\n')
    proxyDomainsText.value = (cfg.proxy_domains || []).join('\n')
    socksAllowText.value = (cfg.socks_allow || []).join('\n')
  }
  // 二维码/分享用的地址：优先中继（circuit）地址，否则第一条局域网地址
  const shareAddr = computed(() => addrs.value.find((a) => a.includes('/p2p-circuit')) || addrs.value[0] || '')
  /** 可达性标签：类型与文案（文案随语言切换） */
  const reach = computed(() => {
    const r = fst.self?.reachability
    return r === 'public'
      ? { type: 'success' as const, text: t('fst.settings.reachPublic') }
      : r === 'private'
        ? { type: 'warning' as const, text: t('fst.settings.reachPrivate') }
        : { type: 'info' as const, text: t('fst.settings.reachUnknown') }
  })

  onMounted(async () => {
    s.value = await fetchSettings()
    loadText(s.value)
    name.value = fst.self?.name || ''
  })
  const saveName = async () => (fst.self = await fetchSetName(name.value))
  /** 本机头像的 data URL（Go 返回裸 base64，需补 data: 前缀） */
  const selfAvatar = computed(() => avatarURL(fst.self?.avatar))
  /**
   * 选一张本地图片设置头像（Go 读文件并校验 PNG/JPEG 与大小）。
   * @returns Promise<void> 无返回；未选到图片时忽略
   */
  const pickAvatar = async () => {
    const paths = await fetchPickFiles()
    const img = paths.find((p) => /\.(png|jpe?g)$/i.test(p))
    if (!img) return
    fst.self = await fetchSetAvatar(img)
  }
  /**
   * 清除本机头像（Go 会向已连接设备重发 HELLO）。
   * @returns Promise<void> 无返回
   */
  const clearAvatar = async () => {
    fst.self = await fetchSetAvatar('')
  }
  const save = async () => {
    if (!s.value) return
    s.value.relays = parseLines(relaysText.value)
    s.value.proxy_routes = parseLines(proxyRoutesText.value)
    s.value.proxy_domains = parseLines(proxyDomainsText.value)
    s.value.socks_allow = parseLines(socksAllowText.value)
    s.value = await fetchSaveSettings(s.value)
    loadText(s.value)
  }
  const pick = async () => {
    const d = await fetchPickDir()
    if (d && s.value) s.value.download_dir = d
  }
  const connect = async () => {
    await fetchConnect(addr.value.trim())
    addr.value = ''
  }
  /**
   * 复制文本到剪贴板并提示。
   * @param text 待复制文本
   * @returns Promise<void> 无返回
   */
  const copy = async (text: string) => {
    await navigator.clipboard.writeText(text)
    ElMessage.success(t('fst.common.copied'))
  }
</script>