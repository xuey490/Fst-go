package com.fst.phone.data

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json

/**
 * Go 核心与 Kotlin 之间约定的 JSON 解码器。
 *
 * 说明：
 *  - ignoreUnknownKeys：Go 端字段增加时不崩，旧客户端仍能用。
 *  - coerceInputValues：把 `null` 当成「未提供」，回落到属性默认值。Go 的
 *    nil 切片会被编码成 `null`（例如没有附件的消息 `"attach":null`），
 *    kotlinx.serialization 默认会把它当作类型错误直接抛异常，一条消息就让
 *    整个会话解码失败、界面永远空着——这正是「手机端没有聊天记录」的根因。
 *  - isLenient / explicitNulls：容忍历史数据里的松散格式。
 */
val FstJson: Json = Json {
    ignoreUnknownKeys = true
    coerceInputValues = true
    isLenient = true
    explicitNulls = false
}

/**
 * 核心进程的生命周期状态。
 *
 * 界面必须能区分「还在启动」与「启动失败」，否则两种情况在 UI 上都表现为
 * 「正在搜索局域网设备…」，无法定位问题。
 */
enum class CoreState {
    /** 尚未启动（服务未运行）。 */
    STOPPED,

    /** 已拉起子进程，等待 LISTEN 与 /health。 */
    STARTING,

    /** 核心就绪，libp2p 已监听 8787。 */
    RUNNING,

    /** 拉起或健康检查失败，[CoreStatus.detail] 是原因。 */
    FAILED,
}

/**
 * 核心进程状态快照，供界面展示与问题定位。
 *
 * @property state 当前生命周期阶段
 * @property detail 人可读的补充说明（端口、PeerID 前 8 位、失败原因）
 * @property logTail 核心 stdout 的最近若干行（含 mdns registered / 错误栈）
 */
data class CoreStatus(
    val state: CoreState = CoreState.STOPPED,
    val detail: String = "",
    val logTail: List<String> = emptyList(),
)

/** 本机信息，对应 Go 的 SelfJSON。 */
@Serializable
data class SelfInfo(
    @SerialName("peer_id") val peerId: String = "",
    val name: String = "",
    val version: String = "",
    /** libp2p 实际 TCP 监听端口（8787 被占用时会回落），用于 DNS-SD 的 SRV 记录。 */
    val port: Int = 0,
    /** 对外广播的本机 IPv4；空表示核心还没拿到地址（此时 addrs 里是回环）。 */
    @SerialName("lan_ip") val lanIp: String = "",
    val addrs: List<String> = emptyList(),
    @SerialName("download_dir") val downloadDir: String = "",
    /** 本机头像的 base64（≤16KB），未设置为空串。 */
    val avatar: String = "",
)

/**
 * 设置页的可配置项，对应 Go 的 SettingsJSON。
 *
 * 字段语义与桌面端 settings 表一致：并发/限速/预校验/自动接收由 transfer 服务
 * 在使用时重新读取，保存即生效；桌面壳专属项（开机自启、关闭行为、单实例、
 * 跨网络中继、代理）在移动端没有对应实现，因此这里不出现。
 */
@Serializable
data class SettingsInfo(
    val name: String = "",
    val avatar: String = "",
    @SerialName("download_dir") val downloadDir: String = "",
    @SerialName("data_dir") val dataDir: String = "",
    val concurrency: Int = 3,
    @SerialName("rate_limit_kbps") val rateLimitKbps: Int = 0,
    val prehash: Boolean = false,
    @SerialName("auto_accept") val autoAccept: Boolean = false,
    val version: String = "",
    @SerialName("protocol_version") val protocolVersion: String = "",
)

/** 已发现的对端，对应 node.PeerView。 */
@Serializable
data class PeerInfo(
    @SerialName("peer_id") val peerId: String = "",
    val name: String = "",
    val platform: String = "",
    val online: Boolean = false,
    val relayed: Boolean = false,
    val ip: String = "",
    val addrs: List<String> = emptyList(),
    val avatar: String = "",
    @SerialName("last_seen") val lastSeen: Long = 0,
    val trusted: Boolean = false,
) {
    /** 界面展示名：空名字回落到 PeerID 前 8 位。 */
    val display: String
        get() = name.ifBlank { peerId.take(8) }
}

/** 单条聊天消息，对应 store.Message。 */
@Serializable
data class ChatMessage(
    val id: String = "",
    @SerialName("peer_id") val peerId: String = "",
    val direction: String = "",
    val content: String = "",
    val status: String = "",
    val ts: Long = 0,
    val read: Boolean = false,
    val kind: String = "text",
    val attach: List<String> = emptyList(),
    val recalled: Boolean = false,
) {
    /** 是否自己发出的（右侧气泡）。 */
    val mine: Boolean get() = direction == "out"
}

/** 会话摘要，对应 store.Conversation。 */
@Serializable
data class Conversation(
    @SerialName("peer_id") val peerId: String = "",
    @SerialName("last_content") val lastContent: String = "",
    @SerialName("last_ts") val lastTs: Long = 0,
    val unread: Int = 0,
    @SerialName("last_kind") val lastKind: String = "",
    @SerialName("last_recalled") val lastRecalled: Boolean = false,
)

/** 一条传输记录，对应 store.Transfer。 */
@Serializable
data class TransferItem(
    val id: String = "",
    @SerialName("batch_id") val batchId: String = "",
    @SerialName("peer_id") val peerId: String = "",
    val direction: String = "",
    @SerialName("file_name") val fileName: String = "",
    @SerialName("rel_path") val relPath: String = "",
    val size: Long = 0,
    val sha256: String = "",
    @SerialName("local_path") val localPath: String = "",
    val status: String = "",
    @SerialName("bytes_done") val bytesDone: Long = 0,
    val error: String = "",
    @SerialName("created_at") val createdAt: Long = 0,
    @SerialName("finished_at") val finishedAt: Long = 0,
) {
    /** 进度百分比 0..1，未知大小时返回 0。 */
    val progress: Float
        get() = if (size <= 0) 0f else (bytesDone.toFloat() / size).coerceIn(0f, 1f)

    val incoming: Boolean get() = direction == "in"
}

/** 传输列表分页包装，对应 TransfersJSON 的 {"total","items"}。 */
@Serializable
data class TransferPage(
    val total: Int = 0,
    val items: List<TransferItem> = emptyList(),
)

/** 一次 incoming 文件请求，对应 transfer:offer 事件载荷。 */
@Serializable
data class FileOffer(
    @SerialName("batch_id") val batchId: String = "",
    @SerialName("peer_id") val peerId: String = "",
    @SerialName("peer_name") val peerName: String = "",
    val auto: Boolean = false,
    val files: List<TransferItem> = emptyList(),
)
