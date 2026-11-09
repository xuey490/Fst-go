package com.fst.phone.data

import android.content.Context
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.net.Uri
import android.net.wifi.WifiManager
import android.provider.OpenableColumns
import android.util.Base64
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import kotlinx.serialization.decodeFromString
import kotlinx.serialization.json.add
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import kotlinx.serialization.json.putJsonArray
import java.io.ByteArrayOutputStream
import java.io.File
import java.net.URLEncoder
import kotlin.concurrent.thread

/**
 * FstRepository 是「Go 核心进程 ↔ Compose 界面」的唯一桥。
 *
 * 与早期的 gomobile 版本相比，核心变成独立进程（见 [FstProcess]），本类职责变成：
 *  1. 拉起核心并持有 [CoreClient]。
 *  2. 用系统 DNS-SD（[Nsd]）做局域网发现：Android 11+ 禁止 Go 侧枚举网卡，
 *     发现只能由系统代劳，解析到的 multiaddr 再注入核心。
 *  3. 订阅 SSE 事件流，转换成 Compose 可订阅的 StateFlow。
 *  4. 提供界面动作：发消息、选文件发送、接收/拒收、导出到「下载」。
 *
 * 线程模型：网络与磁盘都在 IO 线程；SSE 读取在独立线程，回调里只投递事件。
 */
object FstRepository {

    /** 事件驱动刷新的防抖间隔。 */
    private const val REFRESH_DEBOUNCE_MS = 250L

    /** 已注册 DNS-SD 后，复查本机 IP 的间隔（换 Wi-Fi、DHCP 续租会变）。 */
    private const val NETWORK_WATCH_MS = 15_000L

    /**
     * 周期重播本机服务的间隔。
     *
     * 部分 ROM 的 `mdnsd` 收不到组播查询后就不再应答，对端列表里会「凭空少一台」。
     * 主动重播不依赖收包，能把本机重新推回所有人的列表，代价是每轮一次注册。
     */
    private const val REANNOUNCE_MS = 180_000L

    /** 启动阶段等待 Wi-Fi 与 PeerID 就绪的重试间隔。 */
    private const val STARTUP_RETRY_MS = 3_000L

    /** 日志标签。 */
    private const val TAG = "fst-repo"

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val refreshMutex = Mutex()

    private var client: CoreClient? = null
    private var multicastLock: WifiManager.MulticastLock? = null
    private var nsd: Nsd? = null
    private var started = false
    private var streamThread: Thread? = null

    private val _self = MutableStateFlow(SelfInfo())
    private val _peers = MutableStateFlow<List<PeerInfo>>(emptyList())
    private val _conversations = MutableStateFlow<List<Conversation>>(emptyList())
    private val _transfers = MutableStateFlow<List<TransferItem>>(emptyList())
    private val _messages = MutableStateFlow<List<ChatMessage>>(emptyList())
    private val _offer = MutableStateFlow<FileOffer?>(null)
    private val _notice = MutableSharedFlow<String>(extraBufferCapacity = 8)
    private val _nsdStatus = MutableStateFlow("未注册")
    private val _settings = MutableStateFlow(SettingsInfo())

    val self: StateFlow<SelfInfo> = _self
    val peers: StateFlow<List<PeerInfo>> = _peers
    val conversations: StateFlow<List<Conversation>> = _conversations
    val transfers: StateFlow<List<TransferItem>> = _transfers
    val messages: StateFlow<List<ChatMessage>> = _messages
    val offer: StateFlow<FileOffer?> = _offer
    val notice: SharedFlow<String> = _notice

    /** 设置页的可配置项快照，来自核心的 settings 表。 */
    val settings: StateFlow<SettingsInfo> = _settings

    /** 本机 DNS-SD 广播状态（已注册 / 失败原因），界面直接显示，便于远程排障。 */
    val nsdStatus: StateFlow<String> = _nsdStatus

    /** 核心进程状态：界面用它显示「启动中 / 运行中 / 失败原因」，不依赖 Toast。 */
    val coreStatus: StateFlow<CoreStatus> = FstProcess.status

    /** 当前打开的会话对端；收到新消息时只刷新这一个会话。 */
    private var openPeer: String? = null

    /**
     * 启动核心。重复调用安全。
     *
     * 参数:
     *  - context: Context — 应用上下文
     *  - deviceName: String — 本机显示名，空则用机型
     *
     * 返回:
     *  - Unit；失败通过 notice 提示，不抛异常（前台服务不能因此崩溃）
     */
    fun start(context: Context, deviceName: String = "") {
        if (started) return
        started = true
        val app = context.applicationContext
        scope.launch {
            runCatching {
                // 先取本机 IPv4 再拉起核心：Android 上 Go 枚举不到网卡，只能由
                // 这里把地址传进去，否则对外广播的是 127.0.0.1，对端拨号会连到它自己。
                val ip = Nsd.localIPv4(app).orEmpty()
                val c = FstProcess.ensureStarted(app, deviceName, ip)
                client = c
                acquireMulticastLock(app)
                refreshAll()
                startEventStream(c)
                startDiscovery(app)
            }.onFailure {
                _notice.tryEmit("核心启动失败：${it.message}")
                started = false
            }
        }
    }

    /** 关闭核心、注销 DNS-SD 并释放组播锁。由前台服务在 onDestroy 调用。 */
    fun stop() {
        started = false
        runCatching { nsd?.stop() }
        nsd = null
        _nsdStatus.value = "未注册"
        streamThread?.interrupt()
        streamThread = null
        client = null
        FstProcess.stop()
        runCatching { multicastLock?.let { if (it.isHeld) it.release() } }
        multicastLock = null
    }

    /**
     * 启动系统 DNS-SD 发现，并持续盯着本机 IPv4 的变化。
     *
     * 本机地址取自 ConnectivityManager（不经 netlink），端口取自 `/self` 的
     * `port` 字段（8787 被占用时会回落）。IPv4 一旦变化（换 Wi-Fi、DHCP 续租），
     * 会重新向核心下发广播地址并重新注册服务，避免对端按旧地址拨号。
     *
     * 参数:
     *  - context: Context — 应用上下文
     */
    private fun startDiscovery(context: Context) {
        scope.launch {
            var registeredIP = ""
            var attempts = 0
            var lastAnnounce = 0L
            while (started) {
                val me = _self.value
                val ip = Nsd.localIPv4(context)
                if (me.peerId.isNotBlank() && me.port > 0 && !ip.isNullOrBlank()) {
                    attempts = 0
                    if (ip != registeredIP) {
                        applyLanIP(ip)
                        registerNsd(context, ip, me)
                        registeredIP = ip
                        lastAnnounce = System.currentTimeMillis()
                        refreshAll()
                    } else if (System.currentTimeMillis() - lastAnnounce >= REANNOUNCE_MS) {
                        // 地址没变也要定期重播：手机 mdnsd 可能已经收不到组播查询，
                        // 只有主动广播才能让对端继续看见本机（见 REANNOUNCE_MS 注释）。
                        nsd?.announceAgain()
                        lastAnnounce = System.currentTimeMillis()
                    }
                    kotlinx.coroutines.delay(NETWORK_WATCH_MS)
                } else {
                    if (++attempts == 10) _notice.tryEmit("未取得本机 Wi-Fi 地址，局域网发现未启动")
                    kotlinx.coroutines.delay(STARTUP_RETRY_MS)
                }
            }
        }
    }

    /**
     * 把本机 IPv4 交给核心，用于改写对外广播的地址。
     *
     * 参数:
     *  - ip: String — 形如 `192.168.1.13` 的地址
     */
    private suspend fun applyLanIP(ip: String) {
        runCatching {
            val c = client ?: return
            val json = buildJsonObject { put("ip", ip) }.toString()
            c.errorOf(c.post("/lanip", json))?.let { android.util.Log.w(TAG, "更新广播地址失败：$it") }
        }
    }

    /**
     * 注册 `_fst._tcp` 服务并浏览局域网；会先停掉上一轮注册。
     *
     * 参数:
     *  - context: Context — 应用上下文
     *  - ip: String — 本机 IPv4，写进 TXT 的 dnsaddr
     *  - me: SelfInfo — 本机 PeerID 与端口
     */
    private fun registerNsd(context: Context, ip: String, me: SelfInfo) {
        runCatching { nsd?.stop() }
        val selfAddr = "/ip4/$ip/tcp/${me.port}/p2p/${me.peerId}"
        val service = Nsd(context)
        nsd = service
        service.start(
            // 实例名带完整 PeerID：TXT 被 ROM 吞掉时，对端仍能靠实例名 + SRV + A
            // 记录还原出可拨号地址（见 Nsd.fallbackAddr / lanmdns.fallbackInfo）。
            instanceName = "FST-" + me.peerId,
            port = me.port,
            selfAddr = selfAddr,
            onPeer = { addr -> submitDiscovered(addr) },
            onStatus = { msg ->
                // 状态常驻显示（已注册 / 失败 code），失败另外弹一次提示。
                _nsdStatus.value = msg.removePrefix("DNS-SD ")
                if (!msg.startsWith("DNS-SD 已注册")) _notice.tryEmit(msg)
            },
        )
    }

    /**
     * 把系统 DNS-SD 解析到的对端地址注入 Go 核心并触发连接。
     *
     * 参数:
     *  - addr: String — 形如 `/ip4/192.168.1.15/tcp/8787/p2p/12D3KooW...`
     */
    private fun submitDiscovered(addr: String) {
        scope.launch {
            runCatching {
                val c = client ?: return@launch
                val json = buildJsonObject { put("addr", addr) }.toString()
                c.errorOf(c.post("/discovered", json))?.let {
                    android.util.Log.w("fst-nsd", "注入地址失败：$it")
                }
            }
        }
    }

    /**
     * 停止后重新拉起核心，供界面上的「重试」按钮使用。
     *
     * 参数:
     *  - context: Context — 应用上下文
     *  - deviceName: String — 本机显示名，空则用机型
     *
     * 返回:
     *  - Unit；结果体现在 [coreStatus] 与 [peers]
     */
    fun restart(context: Context, deviceName: String = "") {
        stop()
        start(context, deviceName)
    }

    /**
     * 获取 MulticastLock。
     *
     * 发现现在由系统 `NsdManager`（`mdnsd` 进程）负责，理论上不再需要应用自己
     * 持组播锁；保留它是为了两件事：厂 ROM 上偶尔会掐掉 mdnsd 的后台组播，以及
     * 后续若恢复 Go 侧直连 mDNS 时的兼容。拿不到锁不致命，只提示。
     *
     * 参数:
     *  - context: Context — 应用上下文
     */
    private fun acquireMulticastLock(context: Context) {
        runCatching {
            val wifi = context.getSystemService(Context.WIFI_SERVICE) as WifiManager
            val lock = wifi.createMulticastLock("fst-mdns")
            lock.setReferenceCounted(false)
            lock.acquire()
            multicastLock = lock
        }.onFailure { _notice.tryEmit("无法获取组播锁，设备发现可能不完整") }
    }

    /**
     * 启动 SSE 事件线程；断线自动重连（核心进程重启后可自愈）。
     *
     * 参数:
     *  - c: CoreClient — 已就绪的客户端
     */
    private fun startEventStream(c: CoreClient) {
        streamThread?.interrupt()
        streamThread = thread(isDaemon = true, name = "fst-events") {
            while (started && !Thread.currentThread().isInterrupted) {
                runCatching { c.streamEvents(::onEvent) }
                if (!started) break
                Thread.sleep(1_000)
            }
        }
    }

    /** 处理一个核心事件：文件请求单独解析，其余只触发刷新。 */
    private fun onEvent(name: String, payload: String) {
        when (name) {
            "transfer:offer" -> {
                runCatching { FstJson.decodeFromString<FileOffer>(payload) }
                    .onSuccess { _offer.value = it }
                scheduleRefresh(0)
            }
            "chat:message", "chat:ack", "chat:recall" -> {
                openPeer?.let { peer -> scope.launch { refreshMessages(peer) } }
                scheduleRefresh(REFRESH_DEBOUNCE_MS)
            }
            else -> scheduleRefresh(REFRESH_DEBOUNCE_MS)
        }
    }

    /** 合并短时间内的多次事件，只做一次全量刷新。 */
    private fun scheduleRefresh(delayMs: Long) {
        scope.launch {
            if (delayMs > 0) kotlinx.coroutines.delay(delayMs)
            refreshAll()
        }
    }

    /** 读取一次全量状态：本机信息、设备、会话、传输、设置。 */
    private suspend fun refreshAll() = withContext(Dispatchers.IO) {
        val c = client ?: return@withContext
        refreshMutex.withLock {
            runCatching { FstJson.decodeFromString<SelfInfo>(c.get("/self")) }
                .onSuccess { _self.value = it }
            runCatching { FstJson.decodeFromString<List<PeerInfo>>(c.get("/peers")) }
                .onSuccess { _peers.value = it }
            runCatching { FstJson.decodeFromString<List<Conversation>>(c.get("/conversations")) }
                .onSuccess { _conversations.value = it }
            runCatching { FstJson.decodeFromString<TransferPage>(c.get("/transfers?limit=100")) }
                .onSuccess { _transfers.value = it.items }
            runCatching { FstJson.decodeFromString<SettingsInfo>(c.get("/settings")) }
                .onSuccess { _settings.value = it }
        }
    }

    /**
     * 保存设置项。
     *
     * 参数:
     *  - name: String — 本机显示名（空串表示不改）
     *  - concurrency: Int — 并行传输文件数 1–8
     *  - rateLimitKbps: Int — 全局限速 KB/s，0 表示不限速
     *  - prehash: Boolean — 发送前先算整文件 SHA-256
     *  - autoAccept: Boolean — 对所有设备跳过接收确认
     *
     * 返回:
     *  - Unit；失败与成功都通过 notice 提示，界面不阻塞
     */
    fun saveSettings(
        name: String,
        concurrency: Int,
        rateLimitKbps: Int,
        prehash: Boolean,
        autoAccept: Boolean,
    ) {
        scope.launch {
            runCatching {
                val json = buildJsonObject {
                    put("name", name.trim())
                    put("concurrency", concurrency)
                    put("rate_limit_kbps", rateLimitKbps)
                    put("prehash", prehash)
                    put("auto_accept", autoAccept)
                }.toString()
                val resp = client?.post("/settings", json) ?: return@launch
                val err = client?.errorOf(resp)
                _notice.tryEmit(err?.let { "保存失败：$it" } ?: "已保存")
            }.onFailure { _notice.tryEmit("保存失败：${it.message}") }
            refreshAll()
        }
    }

    /**
     * 从系统图片选择器设置本机头像。
     *
     * 参数:
     *  - context: Context — 用于 ContentResolver
     *  - uri: Uri — 选中的图片
     *
     * 说明：图片在本机压到 ≤12KB 后再 base64 上传——头像随每轮 HELLO 同步，
     * 太大既拖慢握手，也会被核心按 16KB 上限拒绝。
     */
    fun setAvatar(context: Context, uri: Uri) {
        scope.launch {
            runCatching {
                val b64 = withContext(Dispatchers.IO) { compressAvatar(context, uri) }
                    ?: error("无法读取所选图片")
                val resp = client?.post("/avatar", buildJsonObject { put("b64", b64) }.toString())
                    ?: return@launch
                val err = client?.errorOf(resp)
                _notice.tryEmit(err?.let { "头像设置失败：$it" } ?: "头像已更新")
            }.onFailure { _notice.tryEmit("头像设置失败：${it.message}") }
            refreshAll()
        }
    }

    /** 清除本机头像。 */
    fun clearAvatar() = setAvatarB64("")

    /** 直接提交 base64 头像，空串表示清除。 */
    private fun setAvatarB64(b64: String) {
        scope.launch {
            runCatching {
                val resp = client?.post("/avatar", buildJsonObject { put("b64", b64) }.toString())
                    ?: return@launch
                client?.errorOf(resp)?.let { _notice.tryEmit("操作失败：$it") }
            }.onFailure { _notice.tryEmit("操作失败：${it.message}") }
            refreshAll()
        }
    }

    /**
     * 切换设备信任状态；受信设备的文件请求会被自动接收。
     *
     * 参数:
     *  - peerId: String — 对方 PeerID
     *  - trusted: Boolean — 是否信任
     */
    fun setTrusted(peerId: String, trusted: Boolean) {
        scope.launch {
            runCatching {
                val json = buildJsonObject {
                    put("peer", peerId)
                    put("trusted", trusted)
                }.toString()
                val resp = client?.post("/trust", json) ?: return@launch
                client?.errorOf(resp)?.let { _notice.tryEmit(it) }
                    ?: _notice.tryEmit(if (trusted) "已信任该设备" else "已取消信任")
            }.onFailure { _notice.tryEmit("操作失败：${it.message}") }
            refreshAll()
        }
    }

    /**
     * 移除一台离线设备记录（重装后的旧身份会以同名离线设备残留）。
     *
     * 参数:
     *  - peerId: String — 对方 PeerID；在线设备会被核心拒绝
     */
    fun removePeer(peerId: String) {
        scope.launch {
            runCatching {
                val resp = client?.post("/peers/remove", buildJsonObject { put("peer", peerId) }.toString())
                    ?: return@launch
                client?.errorOf(resp)?.let { _notice.tryEmit(it) } ?: _notice.tryEmit("已移除设备")
            }.onFailure { _notice.tryEmit("操作失败：${it.message}") }
            refreshAll()
        }
    }

    /** 清理已结束的传输记录（文件本身不删）。 */
    fun clearTransfers() {
        scope.launch {
            runCatching {
                val resp = client?.post("/transfers/clear", "{}") ?: return@launch
                val err = client?.errorOf(resp)
                _notice.tryEmit(err?.let { "清理失败：$it" } ?: "已清理传输记录")
            }.onFailure { _notice.tryEmit("清理失败：${it.message}") }
            refreshAll()
        }
    }

    /**
     * 把任意图片压成正方形 JPEG 并编码成 base64。
     *
     * 参数:
     *  - context: Context — 用于 ContentResolver
     *  - uri: Uri — 源图片
     *
     * 返回:
     *  - String? — base64（无 data: 前缀）；读取或解码失败时 null
     *
     * 策略：先按 2 的幂采样解码（避免大图 OOM），再中心裁成正方形，然后逐级
     * 缩小尺寸与质量，直到 base64 长度 ≤ 15000（核心上限 16000）。
     */
    private fun compressAvatar(context: Context, uri: Uri): String? {
        val raw = runCatching {
            context.contentResolver.openInputStream(uri)?.use { it.readBytes() }
        }.getOrNull() ?: return null
        val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        BitmapFactory.decodeByteArray(raw, 0, raw.size, bounds)
        if (bounds.outWidth <= 0 || bounds.outHeight <= 0) return null
        var sample = 1
        while (bounds.outWidth / (sample * 2) >= 256 && bounds.outHeight / (sample * 2) >= 256) sample *= 2
        val src = BitmapFactory.decodeByteArray(raw, 0, raw.size, BitmapFactory.Options().apply { inSampleSize = sample })
            ?: return null
        val side = minOf(src.width, src.height)
        val square = Bitmap.createBitmap(src, (src.width - side) / 2, (src.height - side) / 2, side, side)
        var target = 192
        while (target >= 64) {
            val scaled = Bitmap.createScaledBitmap(square, target, target, true)
            for (q in intArrayOf(88, 72, 56, 40)) {
                val out = ByteArrayOutputStream()
                scaled.compress(Bitmap.CompressFormat.JPEG, q, out)
                val b64 = Base64.encodeToString(out.toByteArray(), Base64.NO_WRAP)
                if (b64.length <= 15000) return b64
            }
            target -= 32
        }
        return null
    }

    /**
     * 打开/切换会话：加载历史并标记已读。
     *
     * 参数:
     *  - peerId: String? — 对方 PeerID；null 表示关闭会话
     */
    fun openChat(peerId: String?) {
        openPeer = peerId
        // 先清空：否则切换到没有历史的会话时，会短暂显示上一个会话的消息。
        _messages.value = emptyList()
        if (peerId == null) return
        scope.launch {
            refreshMessages(peerId)
            runCatching { client?.post("/markread", buildJsonObject { put("peer", peerId) }.toString()) }
            refreshAll()
        }
    }

    /**
     * 读取某个会话最近 200 条消息并按时间正序排列。
     *
     * 参数:
     *  - peerId: String — 对方 PeerID
     *
     * 说明：只有仍停留在该会话时才写回 [_messages]，避免快速切换会话时
     * 旧请求覆盖新列表；解码失败会打日志（此前被 runCatching 静默吞掉，
     * 表现就是「消息明明发出去了，界面上一条都没有」）。
     */
    private suspend fun refreshMessages(peerId: String) = withContext(Dispatchers.IO) {
        val c = client ?: return@withContext
        val q = URLEncoder.encode(peerId, "UTF-8")
        val raw = runCatching { c.get("/messages?peer=$q&limit=200") }.getOrElse {
            android.util.Log.w(TAG, "读取会话失败 peer=${peerId.take(8)}", it)
            return@withContext
        }
        val list = runCatching { FstJson.decodeFromString<List<ChatMessage>>(raw) }
            .onFailure { android.util.Log.w(TAG, "消息解析失败：${raw.take(200)}", it) }
            .getOrElse { return@withContext }
        if (openPeer == peerId) _messages.value = list.sortedBy { it.ts }
    }

    /**
     * 发送文本消息。
     *
     * 参数:
     *  - peerId: String — 对方 PeerID
     *  - text: String — 内容（首尾空白会被去掉）
     */
    fun sendText(peerId: String, text: String) {
        val body = text.trim()
        if (body.isEmpty()) return
        scope.launch {
            runCatching {
                val json = buildJsonObject { put("peer", peerId); put("text", body) }.toString()
                val resp = client?.post("/send", json) ?: return@launch
                client?.errorOf(resp)?.let { _notice.tryEmit("发送失败：$it") }
            }.onFailure { _notice.tryEmit("发送失败：${it.message}") }
            refreshMessages(peerId)
            refreshAll()
        }
    }

    /**
     * 把 SAF 选中的文档复制到私有发件箱，再发起传输。
     *
     * 参数:
     *  - context: Context — 用于 ContentResolver
     *  - peerId: String — 对方 PeerID
     *  - uris: List<Uri> — SAF 返回的文档 URI
     *
     * 副作用：在 cacheDir/outbox 写入临时副本（发送期间占用一份磁盘空间）。
     */
    fun sendDocuments(context: Context, peerId: String, uris: List<Uri>) {
        if (uris.isEmpty()) return
        scope.launch {
            val outbox = File(context.cacheDir, "outbox").apply { mkdirs() }
            val paths = mutableListOf<String>()
            uris.forEach { uri ->
                runCatching {
                    val name = displayName(context, uri)
                    val target = uniqueFile(outbox, name)
                    context.contentResolver.openInputStream(uri)?.use { input ->
                        target.outputStream().use { output -> input.copyTo(output, 64 * 1024) }
                    } ?: error("无法读取所选文件")
                    paths += target.absolutePath
                }.onFailure { _notice.tryEmit("跳过 ${it.message}") }
            }
            if (paths.isEmpty()) return@launch
            runCatching {
                val json = buildJsonObject {
                    put("peer", peerId)
                    putJsonArray("paths") { paths.forEach { add(it) } }
                }.toString()
                val resp = client?.post("/sendfiles", json) ?: return@launch
                client?.errorOf(resp)?.let { _notice.tryEmit("发送失败：$it") }
            }.onFailure { _notice.tryEmit("发送失败：${it.message}") }
            refreshAll()
        }
    }

    /** 接受待确认的文件批次。 */
    fun acceptOffer(batchId: String) {
        _offer.value = null
        decide("/accept", batchId)
    }

    /** 拒绝待确认的文件批次。 */
    fun rejectOffer(batchId: String) {
        _offer.value = null
        decide("/reject", batchId)
    }

    /** 调用 /accept 或 /reject。 */
    private fun decide(path: String, batchId: String) {
        scope.launch {
            runCatching {
                val json = buildJsonObject { put("batch", batchId) }.toString()
                val resp = client?.post(path, json) ?: return@launch
                client?.errorOf(resp)?.let { _notice.tryEmit(it) }
            }.onFailure { _notice.tryEmit("操作失败：${it.message}") }
            refreshAll()
        }
    }

    /**
     * 把已接收的文件导出到系统「下载/FST」。
     *
     * 参数:
     *  - context: Context — 用于 MediaStore
     *  - item: TransferItem — 已完成的传输记录
     */
    fun export(context: Context, item: TransferItem) {
        scope.launch {
            runCatching { Export.copyToDownloads(context, File(item.localPath), item.fileName) }
                .onSuccess { _notice.tryEmit("已保存到 下载/FST/${item.fileName}") }
                .onFailure { _notice.tryEmit("导出失败：${it.message}") }
        }
    }

    /** 清空当前的文件请求提示。 */
    fun dismissOffer() {
        _offer.value = null
    }

    /** 取 SAF 文档显示名，取不到时用时间戳兜底。 */
    private fun displayName(context: Context, uri: Uri): String {
        runCatching {
            context.contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)
                ?.use { cursor ->
                    if (cursor.moveToFirst()) {
                        val idx = cursor.getColumnIndex(OpenableColumns.DISPLAY_NAME)
                        if (idx >= 0) {
                            val name = cursor.getString(idx)
                            if (!name.isNullOrBlank()) return name
                        }
                    }
                }
        }
        return "file-${System.currentTimeMillis()}"
    }

    /** 在目录里生成不重名的文件路径。 */
    private fun uniqueFile(dir: File, name: String): File {
        val candidate = File(dir, name)
        if (!candidate.exists()) return candidate
        val dot = name.lastIndexOf('.')
        val base = if (dot > 0) name.substring(0, dot) else name
        val ext = if (dot > 0) name.substring(dot) else ""
        var i = 1
        while (true) {
            val next = File(dir, "$base-$i$ext")
            if (!next.exists()) return next
            i++
        }
    }
}
