package com.fst.phone.data

import android.content.Context
import java.io.File
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit
import kotlin.concurrent.thread
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * FstProcess 负责拉起与关闭 Go 核心守护进程（libfstcored.so）。
 *
 * 为什么二进制放在 jniLibs：APK 只有 nativeLibraryDir 下的文件在 Android 10+
 * 仍然允许 execve（应用私有可写目录里的可执行文件被 W^X 策略禁止）。把
 * Go 编译成 lib*.so 放进 jniLibs，安装时系统会解压到 nativeLibraryDir，
 * 于是既能执行，也不需要任何存储权限。
 *
 * 前提：Gradle 必须开启 `useLegacyPackaging = true`，否则原生库不落盘、只留在
 * APK 里，路径不存在也就无法执行。
 */
object FstProcess {

    /** 二进制文件名：必须是 lib*.so 形式，系统才会解压到 nativeLibraryDir。 */
    private const val EXEC_NAME = "libfstcored.so"

    /** 等待守护进程打印 LISTEN 的最长时间。 */
    private const val START_TIMEOUT_MS = 20_000L

    /** 等待核心就绪（/health 返回 ok）的最长时间。 */
    private const val HEALTH_TIMEOUT_MS = 20_000L

    /** 保留的核心 stdout 行数上限，用于失败时在界面展示现场。 */
    private const val LOG_TAIL = 120

    private val _status = MutableStateFlow(CoreStatus())

    /** 核心生命周期状态；界面订阅它区分「启动中 / 运行中 / 失败」。 */
    val status: StateFlow<CoreStatus> = _status.asStateFlow()

    /** 核心 stdout 的环形缓冲，只保留最近 [LOG_TAIL] 行。 */
    private val logLines = ArrayDeque<String>()

    private var process: Process? = null
    private var client: CoreClient? = null

    /**
     * 启动守护进程并返回已就绪的客户端；已启动时直接复用。
     *
     * 参数:
     *  - context: Context — 应用上下文（取私有目录与 nativeLibraryDir）
     *  - deviceName: String — 本机显示名，空则用机型
     *  - lanIP: String — 本机 Wi-Fi/以太网 IPv4，作为对外广播地址；空表示暂不广播
     *
     * 返回:
     *  - CoreClient — 已完成 /health 检查的客户端
     *
     * 异常:
     *  - IllegalStateException — 二进制缺失/不可执行、启动超时、核心未就绪；
     *    抛出前会把状态置为 [CoreState.FAILED] 并保留日志尾部，供界面显示
     */
    fun ensureStarted(context: Context, deviceName: String, lanIP: String = ""): CoreClient {
        client?.let { return it }
        _status.value = CoreStatus(CoreState.STARTING, "正在拉起核心进程…")
        return try {
            launchAndWait(context, deviceName, lanIP)
        } catch (t: Throwable) {
            val msg = t.message?.takeIf { it.isNotBlank() } ?: t.javaClass.simpleName
            _status.value = CoreStatus(CoreState.FAILED, msg, snapshot())
            throw t
        }
    }

    /**
     * 真正拉起子进程并等待 LISTEN 与 /health 就绪。
     *
     * 参数:
     *  - context: Context — 应用上下文
     *  - deviceName: String — 本机显示名
     *  - lanIP: String — 本机 IPv4；为空时不设置环境变量，核心按系统枚举结果广播
     *
     * 返回:
     *  - CoreClient — 已就绪客户端
     *
     * 异常:
     *  - IllegalStateException — 二进制缺失、启动超时或核心不健康
     *
     * 说明：lanIP 必须由调用方从 ConnectivityManager 取，不能依赖 Go 侧枚举网卡
     * ——Android 11+ 禁止应用使用 NETLINK_ROUTE，Go 只能拿到 127.0.0.1。
     */
    private fun launchAndWait(context: Context, deviceName: String, lanIP: String): CoreClient {
        val app = context.applicationContext
        val exe = File(app.applicationInfo.nativeLibraryDir, EXEC_NAME)
        if (!exe.exists()) {
            val abis = android.os.Build.SUPPORTED_ABIS.joinToString(",")
            throw IllegalStateException(
                "缺少核心二进制：${exe.absolutePath}（本机 ABI: $abis，APK 仅含 arm64-v8a）",
            )
        }

        val dataDir = File(app.filesDir, "fst").apply { mkdirs() }
        val downloadDir = (app.getExternalFilesDir(null) ?: dataDir).resolve("downloads").apply { mkdirs() }
        val token = randomToken()

        val builder = ProcessBuilder(exe.absolutePath)
            .directory(app.filesDir)
            .redirectErrorStream(true)
        builder.environment().apply {
            put("FST_DATA_DIR", dataDir.absolutePath)
            put("FST_DOWNLOAD_DIR", downloadDir.absolutePath)
            put("FST_DEVICE_NAME", deviceName.ifBlank { defaultName() })
            if (lanIP.isNotBlank()) put("FST_LAN_IP", lanIP)
            put("FST_TOKEN", token)
        }

        val proc = builder.start()
        val lines = LinkedBlockingQueue<String>()

        // 单独线程持续读取 stdout：既拿到 LISTEN 行，也避免子进程写满管道后阻塞。
        thread(isDaemon = true, name = "fstcored-out") {
            runCatching {
                proc.inputStream.bufferedReader(Charsets.UTF_8).forEachLine { line ->
                    if (!line.startsWith("LISTEN ")) {
                        android.util.Log.i("fstcored", line)
                    }
                    record(line)
                    lines.offer(line)
                }
            }
        }

        var port = 0
        val deadline = System.currentTimeMillis() + START_TIMEOUT_MS
        while (System.currentTimeMillis() < deadline) {
            val line = lines.poll(deadline - System.currentTimeMillis(), TimeUnit.MILLISECONDS) ?: break
            if (line.startsWith("LISTEN ")) {
                port = line.removePrefix("LISTEN ").substringAfterLast(':').trim().toIntOrNull() ?: 0
                break
            }
        }
        if (port <= 0) {
            proc.destroy()
            throw IllegalStateException("核心未能在 ${START_TIMEOUT_MS / 1000}s 内就绪（可能端口或权限异常）")
        }

        val c = CoreClient(port, token)
        // 健康检查失败必须把子进程杀掉：否则它会继续占着 8787 与 IPC 端口，
        // 用户点「重试」时会不断堆叠僵尸核心，后续现象更难解释。
        try {
            waitHealthy(c, HEALTH_TIMEOUT_MS)
        } catch (t: Throwable) {
            runCatching { proc.destroy() }
            throw t
        }

        process = proc
        client = c
        _status.value = CoreStatus(CoreState.RUNNING, "IPC 端口 $port", snapshot())
        return c
    }

    /** 记录一行核心输出到环形缓冲，超出上限丢弃最旧一行。 */
    private fun record(line: String) {
        synchronized(logLines) {
            logLines.addLast(line)
            while (logLines.size > LOG_TAIL) logLines.removeFirst()
        }
    }

    /** 取当前日志尾部快照，用于失败时展示现场。 */
    private fun snapshot(): List<String> = synchronized(logLines) { logLines.toList() }

    /** 停止守护进程：先请求优雅退出，超时则强杀。可重复调用。 */
    fun stop() {
        val c = client
        val p = process
        client = null
        process = null
        runCatching { c?.post("/shutdown", "{}") }
        runCatching {
            if (p != null && !p.waitFor(2, TimeUnit.SECONDS)) p.destroy()
        }
        _status.value = CoreStatus(CoreState.STOPPED)
    }

    /**
     * 轮询 /health 直到成功或超时。
     *
     * 参数:
     *  - c: CoreClient — 客户端
     *  - timeoutMs: Long — 最长等待毫秒数
     *
     * 异常:
     *  - IllegalStateException — 超时仍不可用；消息里会带上最后一次失败原因，
     *    否则界面只显示「超时」，无法区分「明文策略拦截」「端口不对」还是
     *    「进程已退出」——这一点在真机排查时非常关键
     */
    private fun waitHealthy(c: CoreClient, timeoutMs: Long) {
        val deadline = System.currentTimeMillis() + timeoutMs
        var lastError = ""
        while (System.currentTimeMillis() < deadline) {
            val result = runCatching { c.get("/health").contains("\"ok\":true") }
            if (result.getOrDefault(false)) return
            result.exceptionOrNull()?.let { e ->
                lastError = e.message?.takeIf { it.isNotBlank() } ?: e.javaClass.simpleName
            }
            Thread.sleep(300)
        }
        val suffix = if (lastError.isBlank()) "" else "（最后错误：$lastError）"
        throw IllegalStateException("核心健康检查超时$suffix")
    }

    /** 生成 32 字节随机令牌（hex），只通过环境变量交给子进程。 */
    private fun randomToken(): String {
        val bytes = ByteArray(32)
        java.security.SecureRandom().nextBytes(bytes)
        return bytes.joinToString("") { "%02x".format(it) }
    }

    /** 默认设备名：品牌 + 机型。 */
    private fun defaultName(): String {
        val manufacturer = android.os.Build.MANUFACTURER?.replaceFirstChar { it.uppercase() } ?: "Android"
        return "$manufacturer ${android.os.Build.MODEL}"
    }
}
