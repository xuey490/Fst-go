package com.fst.phone.data

import android.content.Context
import android.net.ConnectivityManager
import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import android.os.Handler
import android.os.Looper
import android.util.Log
import java.net.Inet4Address
import java.util.ArrayDeque

/**
 * 基于 Android 系统 DNS-SD（NsdManager）的局域网发现。
 *
 * 为什么手机端不用 Go 侧的 mDNS：
 *  - Android 11 起，普通应用 UID 被禁止 bind NETLINK_ROUTE、也禁止 RTM_GETLINK，
 *    Go 的 `net.Interfaces()` 必然返回 `netlinkrib: permission denied`，
 *    于是 zeroconf 无法枚举网卡，libp2p 的地址也会退化成只有回环。
 *  - 系统 `mdnsd` 常驻占用 5353，第三方再 bind 会 EADDRINUSE。
 *  - `NsdManager` 由系统进程代劳，既不需要 netlink，也不需要自己占 5353。
 *
 * 本类只负责「发现」：注册自己的 `_fst._tcp` 服务、浏览同一服务类型并对结果做
 * TXT 解析，把 `dnsaddr=` 里的完整 multiaddr 交给上层。握手、加密、分帧与文件
 * 传输仍然全部由 Go 核心完成，因此与桌面端协议完全一致。
 *
 * 线程说明：NsdManager 的回调在主线程，[onPeer] 也在主线程触发，实现方必须
 * 自行切到 IO 线程再做网络调用。
 */
class Nsd(context: Context) {

    /** 系统 DNS-SD 管理器；个别精简系统可能为 null。 */
    private val manager: NsdManager? = context.applicationContext
        .getSystemService(NsdManager::class.java)

    /** 待解析队列：resolveService 同一时刻只能有一个，必须串行。 */
    private val pending = ArrayDeque<NsdServiceInfo>()

    /** 串行化 [pending] 与 [resolving] 的锁。 */
    private val lock = Any()

    /** 是否有一次解析正在进行。 */
    private var resolving = false

    /** 已投递过的地址与时间，避免同一对端被反复 POST。 */
    private val delivered = HashMap<String, Long>()

    private var registration: NsdManager.RegistrationListener? = null
    private var discovery: NsdManager.DiscoveryListener? = null

    /** 主线程投递器：重注册需要延迟一拍，避开 ROM 的注销竞态。 */
    private val handler = Handler(Looper.getMainLooper())

    /** 注册参数快照，供 [announceAgain] 重播时复用。 */
    private var regName = ""
    private var regPort = 0
    private var regAddr = ""

    /** 是否有一次重播正在进行，避免并发重复注册。 */
    private var regBusy = false

    /** 本机服务最近一次注册成功的时刻（毫秒）；0 表示还没成功过。 */
    @Volatile
    private var registeredAt = 0L

    /** 最近一次注册结果，供界面直接展示。 */
    @Volatile
    private var regStatus = "未注册"

    /** 生命周期标记：true 表示已停止，异步回调里用它避免「停止后又被重播拉起来」。 */
    @Volatile
    private var stopped = true

    /** 发现回调：参数是带 PeerID 的完整 multiaddr。 */
    private var peerSink: ((String) -> Unit)? = null

    /** 状态回调：用于把失败原因显示到界面上。 */
    private var statusSink: ((String) -> Unit)? = null

    /**
     * 开始注册本机服务并浏览局域网内的其他 FST 设备。
     *
     * @param instanceName 服务实例名，必须是 `FST-` + 完整 PeerID（TXT 缺失时用它兜底识别对端）
     * @param port libp2p 实际 TCP 端口；<=0 时只浏览、不注册
     * @param selfAddr 本机可被对端连接的 multiaddr，写入 TXT 的 `dnsaddr=`
     * @param onPeer 发现对端时的回调，在主线程执行
     * @param onStatus 注册/浏览异常时的说明，用于界面提示；可选
     *
     * 副作用：向系统注册一个 `_fst._tcp` 服务，并开启一次服务浏览。
     */
    fun start(
        instanceName: String,
        port: Int,
        selfAddr: String,
        onPeer: (String) -> Unit,
        onStatus: (String) -> Unit = {},
    ) {
        val mgr = manager
        if (mgr == null) {
            onStatus("系统不支持 DNS-SD（NsdManager 不可用）")
            return
        }
        peerSink = onPeer
        statusSink = onStatus
        stopped = false

        if (port > 0 && selfAddr.isNotBlank()) {
            regName = instanceName
            regPort = port
            regAddr = selfAddr
            register(mgr, instanceName, port, selfAddr)
        } else {
            onStatus("DNS-SD 未注册：缺少本机地址或端口")
        }
        browse(mgr)
    }

    /**
     * 重新广播一次本机服务（注销后重新注册）。
     *
     * 为什么要周期性重播：部分 ROM 的系统 `mdnsd` 在息屏/省电后收不到组播查询，
     * 于是对端的浏览永远等不到本机的应答——表现就是「手机明明在线，别人却看不到」。
     * 重播属于主动组播，不依赖收包能力，因此能可靠地把本机重新推到所有人的列表里。
     * 浏览（[discovery]）不受影响，不会打断已发现的设备。
     *
     * 返回:
     *  - Unit；失败只写日志与状态，交给下一次周期重试
     */
    fun announceAgain() {
        val mgr = manager ?: return
        if (regName.isBlank() || regPort <= 0 || regAddr.isBlank()) return
        synchronized(lock) {
            if (regBusy) return
            regBusy = true
        }
        val current = registration
        registration = null
        runCatching { current?.let { mgr.unregisterService(it) } }
        // 注销是异步的：立刻注册在部分 ROM 上会返回 FAILURE_INTERNAL_ERROR(3)。
        handler.postDelayed({
            register(mgr, regName, regPort, regAddr)
            synchronized(lock) { regBusy = false }
        }, REREGISTER_DELAY_MS)
    }

    /** 是否已停止（异步回调里用于放弃后续动作）。 */
    private fun isStopped(): Boolean = stopped

    /**
     * 返回本机服务注册状态，供界面直接显示。
     *
     * 返回:
     *  - String — 形如「已注册」「注册失败（code=3）」「未注册」
     */
    fun registrationStatus(): String = regStatus

    /**
     * 服务注册成功后距今的秒数，用于判断重播是否真的生效。
     *
     * 返回:
     *  - Long — 秒数；从未成功注册时返回 -1
     */
    fun secondsSinceRegistered(): Long {
        val at = registeredAt
        if (at <= 0L) return -1
        return (System.currentTimeMillis() - at) / 1000
    }

    /**
     * 停止浏览并注销服务；可重复调用。
     *
     * 副作用：释放 NsdManager 的监听器，清空待解析队列。
     */
    fun stop() {
        val mgr = manager ?: return
        handler.removeCallbacksAndMessages(null)
        runCatching { discovery?.let { mgr.stopServiceDiscovery(it) } }
        runCatching { registration?.let { mgr.unregisterService(it) } }
        discovery = null
        registration = null
        peerSink = null
        statusSink = null
        regStatus = "未注册"
        stopped = true
        synchronized(lock) {
            pending.clear()
            delivered.clear()
            regBusy = false
        }
    }

    /**
     * 注册本机服务，TXT 里带 `dnsaddr=<multiaddr>`。
     *
     * @param mgr 系统管理器
     * @param instanceName 实例名
     * @param port SRV 端口
     * @param selfAddr 本机 multiaddr
     */
    private fun register(mgr: NsdManager, instanceName: String, port: Int, selfAddr: String) {
        // setAttribute 对 key/value 长度有约束，个别 ROM 会直接抛异常，必须兜住。
        val info = runCatching {
            NsdServiceInfo().apply {
                serviceName = instanceName
                serviceType = SERVICE_TYPE
                setPort(port)
                // 桌面端的 lanmdns 只认以 dnsaddr= 开头的 TXT，与 libp2p 的约定一致。
                setAttribute(TXT_ADDR, selfAddr)
            }
        }.getOrElse {
            Log.w(TAG, "构造服务信息失败", it)
            statusSink?.invoke("DNS-SD 注册参数非法：${it.message}")
            return
        }
        val listener = object : NsdManager.RegistrationListener {
            override fun onServiceRegistered(info: NsdServiceInfo) {
                registeredAt = System.currentTimeMillis()
                regStatus = "已注册"
                Log.i(TAG, "registered ${info.serviceName} port=$port")
                statusSink?.invoke("DNS-SD 已注册")
            }

            override fun onRegistrationFailed(info: NsdServiceInfo, errorCode: Int) {
                regStatus = "注册失败（code=$errorCode）"
                Log.w(TAG, "register failed: ${info.serviceName} code=$errorCode")
                statusSink?.invoke("DNS-SD 注册失败（code=$errorCode）")
                // 失败后自动重试一次：常见原因是上一次注销还没落地（code=3）。
                if (!isStopped()) {
                    handler.postDelayed({ if (!isStopped()) announceAgain() }, RETRY_DELAY_MS)
                }
            }

            override fun onServiceUnregistered(info: NsdServiceInfo) = Unit

            override fun onUnregistrationFailed(info: NsdServiceInfo, errorCode: Int) = Unit
        }
        registration = listener
        runCatching { mgr.registerService(info, NsdManager.PROTOCOL_DNS_SD, listener) }
            .onFailure {
                Log.w(TAG, "registerService 抛出异常", it)
                statusSink?.invoke("DNS-SD 注册异常：${it.message}")
            }
    }

    /**
     * 浏览 `_fst._tcp` 服务。
     *
     * @param mgr 系统管理器
     *
     * 副作用：常驻监听；服务增减由系统回调驱动。
     */
    private fun browse(mgr: NsdManager) {
        val listener = object : NsdManager.DiscoveryListener {
            override fun onDiscoveryStarted(serviceType: String) {
                Log.i(TAG, "discovery started $serviceType")
            }

            override fun onServiceFound(info: NsdServiceInfo) {
                Log.i(TAG, "found ${info.serviceName} ${info.serviceType}")
                synchronized(lock) { pending.addLast(info) }
                pump()
            }

            override fun onServiceLost(info: NsdServiceInfo) {
                Log.i(TAG, "lost ${info.serviceName}")
            }

            override fun onDiscoveryStopped(serviceType: String) {
                Log.i(TAG, "discovery stopped $serviceType")
            }

            override fun onStartDiscoveryFailed(serviceType: String, errorCode: Int) {
                Log.w(TAG, "start discovery failed $serviceType code=$errorCode")
                statusSink?.invoke("DNS-SD 浏览启动失败（code=$errorCode）")
            }

            override fun onStopDiscoveryFailed(serviceType: String, errorCode: Int) = Unit
        }
        discovery = listener
        runCatching { mgr.discoverServices(SERVICE_TYPE, NsdManager.PROTOCOL_DNS_SD, listener) }
            .onFailure {
                Log.w(TAG, "discoverServices 抛出异常", it)
                statusSink?.invoke("DNS-SD 浏览异常：${it.message}")
            }
    }

    /** 串行发起解析：一次只允许一个 resolveService 在飞行。 */
    private fun pump() {
        val mgr = manager ?: return
        val next: NsdServiceInfo
        synchronized(lock) {
            if (resolving) return
            next = pending.pollFirst() ?: return
            resolving = true
        }
        // resolveService 在部分 ROM 上会直接抛 IllegalArgument，必须兜住并释放锁。
        @Suppress("DEPRECATION")
        val started = runCatching {
            mgr.resolveService(next, object : NsdManager.ResolveListener {
                override fun onResolveFailed(info: NsdServiceInfo, errorCode: Int) {
                    Log.w(TAG, "resolve failed ${info.serviceName} code=$errorCode")
                    synchronized(lock) { resolving = false }
                    pump()
                }

                override fun onServiceResolved(info: NsdServiceInfo) {
                    synchronized(lock) { resolving = false }
                    deliver(info)
                    pump()
                }
            })
        }.isSuccess
        if (!started) {
            synchronized(lock) { resolving = false }
        }
    }

    /**
     * 从解析结果中取出 `dnsaddr` 并投递；TXT 缺失时用实例名兜底。
     *
     * 参数:
     *  - info: NsdServiceInfo — 解析成功的服务信息
     *
     * 说明：部分 ROM 的 mdnsd 在 resolve 时会把 TXT 丢掉。此时实例名
     * （`FST-<完整 PeerID>`）+ SRV 端口 + 解析出的主机地址仍足以拼出可拨号的
     * multiaddr，因此不再直接丢弃——否则表现为「服务看得见、永远连不上」。
     */
    private fun deliver(info: NsdServiceInfo) {
        val raw = info.attributes?.get(TXT_ADDR)
        val addr = raw?.let { runCatching { String(it, Charsets.UTF_8) }.getOrNull() }?.trim().orEmpty()
        if (addr.startsWith("/")) {
            emit(addr)
            return
        }
        val fallback = fallbackAddr(info)
        if (fallback != null) {
            emit(fallback)
            return
        }
        statusSink?.invoke("发现 ${info.host}，但缺少 dnsaddr 且实例名不含 PeerID，已跳过")
        Log.w(TAG, "no dnsaddr and unusable name: ${info.serviceName} host=${info.host}")
    }

    /**
     * 由实例名与解析结果拼出 multiaddr（TXT 缺失时的兜底路径）。
     *
     * 参数:
     *  - info: NsdServiceInfo — 已 resolve 的服务信息
     *
     * 返回:
     *  - String? — 形如 `/ip4/192.168.1.13/tcp/8787/p2p/12D3KooW...`；无法还原时返回 null
     */
    private fun fallbackAddr(info: NsdServiceInfo): String? {
        val id = peerIdFromName(info.serviceName) ?: return null
        val ip = info.host?.hostAddress ?: return null
        if (info.port <= 0) return null
        // 只处理 IPv4：IPv6 字面量需要加方括号规则，移动端首版不覆盖。
        if (ip.contains(':')) return null
        return "/ip4/$ip/tcp/${info.port}/p2p/$id"
    }

    /**
     * 去重后把地址交给上层。
     *
     * 参数:
     *  - addr: String — 带 PeerID 的完整 multiaddr
     *
     * 副作用：30 秒内同一地址只投递一次，避免同一对端反复拨号。
     */
    private fun emit(addr: String) {
        val now = System.currentTimeMillis()
        synchronized(lock) {
            val last = delivered[addr]
            if (last != null && now - last < DEDUPE_TTL_MS) return
            delivered[addr] = now
            if (delivered.size > MAX_DELIVERED) {
                delivered.entries.removeIf { now - it.value > DEDUPE_TTL_MS }
            }
        }
        Log.i(TAG, "resolved $addr")
        peerSink?.invoke(addr)
    }

    companion object {
        /** 日志标签。 */
        private const val TAG = "fst-nsd"

        /** 标准 DNS-SD 服务类型；必须与桌面端 lanmdns.StandardService 一致。 */
        private const val SERVICE_TYPE = "_fst._tcp"

        /** TXT 里承载 multiaddr 的键名，与 libp2p/zeroconf 的约定一致。 */
        private const val TXT_ADDR = "dnsaddr"

        /** 注销与重新注册之间的间隔：给系统 mdnsd 时间腾出旧的记录。 */
        private const val REREGISTER_DELAY_MS = 400L

        /** 注册失败后的重试间隔。 */
        private const val RETRY_DELAY_MS = 15_000L

        /** 同一地址的重复投递抑制窗口。 */
        private const val DEDUPE_TTL_MS = 30_000L

        /** 去重表的软上限。 */
        private const val MAX_DELIVERED = 128

        /** 实例名前缀，必须与桌面端 lanmdns.InstancePrefix 一致。 */
        private const val NAME_PREFIX = "FST-"

        /**
         * 从服务实例名里取出完整 PeerID。
         *
         * 实例名形如 `FST-12D3KooW...`（最多 63 字节，符合 DNS-SD 单个标签上限）；
         * mdnsd 在重名时会追加 ` (2)` 之类的后缀，这里一并剥掉。
         *
         * @param name 服务实例名
         * @return 合法 PeerID；格式不符时返回 null
         */
        private fun peerIdFromName(name: String): String? {
            if (!name.startsWith(NAME_PREFIX)) return null
            var id = name.removePrefix(NAME_PREFIX).trim()
            val dup = id.lastIndexOf(" (")
            if (dup > 0 && id.endsWith(")")) id = id.substring(0, dup)
            // libp2p PeerID 的 base58btc 形式：Ed25519 以 12D3KooW 开头，RSA 以 Qm 开头。
            if (id.length !in 46..60) return null
            val ok = (id.startsWith("12D3") || id.startsWith("Qm")) &&
                id.all { it.isLetterOrDigit() }
            return if (ok) id else null
        }

        /**
         * 读取当前网络的本机 IPv4（Wi-Fi 或以太网）。
         *
         * 走 ConnectivityManager 而不是 NetworkInterface：后者在 Android 11+
         * 依赖 netlink，会拿到 permission denied；LinkProperties 由系统下发，
         * 不经过 netlink。
         *
         * @param context 任意上下文
         * @return 形如 `192.168.1.13` 的地址；没有可用网络时返回 null
         */
        fun localIPv4(context: Context): String? = runCatching {
            val cm = context.getSystemService(ConnectivityManager::class.java) ?: return null
            val network = cm.activeNetwork ?: return null
            val props = cm.getLinkProperties(network) ?: return null
            props.linkAddresses
                .map { it.address }
                .firstOrNull { it is Inet4Address && !it.isLoopbackAddress && !it.isLinkLocalAddress }
                ?.hostAddress
        }.getOrNull()
    }
}
