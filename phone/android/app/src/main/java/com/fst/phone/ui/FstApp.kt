package com.fst.phone.ui

import android.widget.Toast
import androidx.activity.compose.BackHandler
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.fst.phone.data.Conversation
import com.fst.phone.data.CoreState
import com.fst.phone.data.CoreStatus
import com.fst.phone.data.FstRepository
import com.fst.phone.data.PeerInfo
import com.fst.phone.data.SelfInfo
import com.fst.phone.data.TransferItem

/** 顶层路由：首页、四个功能页，或某个会话。 */
private sealed interface Route {
    /** 首页（四个大图标入口）。 */
    data object Home : Route

    /** 设备列表：在线 / 离线分组。 */
    data object Devices : Route

    /** 会话列表。 */
    data object Chats : Route

    /** 传输列表（可搜索）。 */
    data object Transfers : Route

    /** 设置。 */
    data object Settings : Route

    /**
     * 会话页。
     *
     * @property peerId 对方 PeerID
     * @property title 顶栏显示的对方名称
     */
    data class Chat(val peerId: String, val title: String) : Route
}

/**
 * FST 移动端根界面。
 *
 * 参数:
 *
 *	无（状态全部来自 FstRepository 的 StateFlow）
 */
@Composable
fun FstApp() {
    val context = LocalContext.current
    val self by FstRepository.self.collectAsStateWithLifecycle()
    val peers by FstRepository.peers.collectAsStateWithLifecycle()
    val conversations by FstRepository.conversations.collectAsStateWithLifecycle()
    val transfers by FstRepository.transfers.collectAsStateWithLifecycle()
    val settings by FstRepository.settings.collectAsStateWithLifecycle()
    val offer by FstRepository.offer.collectAsStateWithLifecycle()
    val coreStatus by FstRepository.coreStatus.collectAsStateWithLifecycle()
    val nsdStatus by FstRepository.nsdStatus.collectAsStateWithLifecycle()

    var route by remember { mutableStateOf<Route>(Route.Home) }

    // 一次性提示（发送失败、保存结果等）
    LaunchedEffect(Unit) {
        FstRepository.notice.collect { msg ->
            Toast.makeText(context, msg, Toast.LENGTH_SHORT).show()
        }
    }

    // 系统返回键：任何子页面都回到首页，会话页额外清掉「当前会话」。
    BackHandler(enabled = route != Route.Home) {
        if (route is Route.Chat) FstRepository.openChat(null)
        route = Route.Home
    }

    offer?.let { o ->
        AlertDialog(
            onDismissRequest = { FstRepository.dismissOffer() },
            title = { Text("收到文件请求") },
            text = {
                Column {
                    Text("${o.peerName.ifBlank { o.peerId.take(8) }} 想发送 ${o.files.size} 个文件")
                    Spacer(Modifier.height(8.dp))
                    o.files.take(6).forEach {
                        Text("· ${it.fileName}（${formatSize(it.size)}）", fontSize = 13.sp)
                    }
                    if (o.files.size > 6) {
                        Text("…等 ${o.files.size} 个文件", fontSize = 13.sp)
                    }
                }
            },
            confirmButton = {
                TextButton(onClick = { FstRepository.acceptOffer(o.batchId) }) { Text("接收") }
            },
            dismissButton = {
                TextButton(onClick = { FstRepository.rejectOffer(o.batchId) }) { Text("拒绝") }
            },
        )
    }

    val openChat: (String, String) -> Unit = { peerId, title ->
        FstRepository.openChat(peerId)
        route = Route.Chat(peerId, title)
    }
    val back: () -> Unit = { route = Route.Home }

    when (val r = route) {
        is Route.Chat -> ChatScreen(
            peerId = r.peerId,
            title = r.title,
            onBack = {
                FstRepository.openChat(null)
                route = Route.Home
            },
        )
        Route.Devices -> DevicesScreen(peers = peers, onBack = back, onOpenChat = openChat)
        Route.Chats -> ChatsScreen(conversations = conversations, peers = peers, onBack = back, onOpenChat = openChat)
        Route.Transfers -> TransfersScreen(transfers = transfers, peers = peers, onBack = back)
        Route.Settings -> SettingsScreen(
            self = self,
            settings = settings,
            peers = peers,
            coreStatus = coreStatus,
            nsdStatus = nsdStatus,
            onBack = back,
        )
        Route.Home -> HomeScreen(
            self = self,
            peers = peers,
            conversations = conversations,
            transfers = transfers,
            coreStatus = coreStatus,
            onOpen = { route = it },
        )
    }
}

/** 首页：本机摘要 + 核心状态 + 四个大图标入口。 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun HomeScreen(
    self: SelfInfo,
    peers: List<PeerInfo>,
    conversations: List<Conversation>,
    transfers: List<TransferItem>,
    coreStatus: CoreStatus,
    onOpen: (Route) -> Unit,
) {
    val context = LocalContext.current
    val online = peers.count { it.online }
    val unread = conversations.sumOf { it.unread }
    val running = transfers.count { it.status == "running" || it.status == "accepted" }

    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    Column {
                        Text("FST", fontWeight = FontWeight.SemiBold)
                        // 副标题必须用白色半透明：顶栏底色是 primary（深绿），
                        // 深灰字在深色底上几乎看不见。
                        Row(
                            verticalAlignment = Alignment.CenterVertically,
                            modifier = Modifier.clickable(enabled = self.peerId.isNotBlank()) {
                                copyText(context, "FST PeerID", self.peerId, "已复制本机 PeerID")
                            },
                        ) {
                            Text(
                                text = self.name.ifBlank {
                                    if (self.peerId.isBlank()) "核心未就绪" else shortPeer(self.peerId)
                                },
                                fontSize = 12.sp,
                                color = Color.White.copy(alpha = 0.92f),
                            )
                            Spacer(Modifier.width(8.dp))
                            Text(
                                text = "· 在线 $online 台",
                                fontSize = 12.sp,
                                color = Color.White.copy(alpha = 0.75f),
                            )
                            if (self.peerId.isNotBlank()) {
                                Spacer(Modifier.width(6.dp))
                                Text(
                                    text = "复制",
                                    fontSize = 11.sp,
                                    color = Color.White.copy(alpha = 0.65f),
                                )
                            }
                        }
                    }
                },
                colors = TopAppBarDefaults.topAppBarColors(
                    containerColor = MaterialTheme.colorScheme.primary,
                    titleContentColor = Color.White,
                ),
            )
        },
    ) { padding ->
        Column(
            modifier = Modifier
                .padding(padding)
                .fillMaxSize()
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 16.dp),
        ) {
            Spacer(Modifier.height(12.dp))
            SelfCard(self = self, online = online)
            Spacer(Modifier.height(12.dp))
            StatusBanner(coreStatus = coreStatus, onClick = { onOpen(Route.Settings) })
            Spacer(Modifier.height(16.dp))

            // 四个大图标入口：一行两个，卡片接近正方形，拇指区一屏可点。
            Row(horizontalArrangement = Arrangement.spacedBy(14.dp)) {
                HomeTile(
                    title = "设备",
                    subtitle = when {
                        online > 0 -> "在线 $online 台"
                        peers.isNotEmpty() -> "发现 ${peers.size} 台"
                        else -> "搜索中…"
                    },
                    accent = Color(0xFF1B5E4B),
                    modifier = Modifier.weight(1f).aspectRatio(0.94f),
                    onClick = { onOpen(Route.Devices) },
                ) { tint -> DeviceGlyph(tint, 42.dp) }
                HomeTile(
                    title = "会话",
                    subtitle = if (unread > 0) "$unread 条未读" else "${conversations.size} 个会话",
                    accent = Color(0xFF2E6F9E),
                    modifier = Modifier.weight(1f).aspectRatio(0.94f),
                    onClick = { onOpen(Route.Chats) },
                ) { tint -> ChatGlyph(tint, 42.dp) }
            }
            Spacer(Modifier.height(14.dp))
            Row(horizontalArrangement = Arrangement.spacedBy(14.dp)) {
                HomeTile(
                    title = "传输",
                    subtitle = if (running > 0) "$running 个进行中" else "${transfers.size} 条记录",
                    accent = Color(0xFFB4691B),
                    modifier = Modifier.weight(1f).aspectRatio(0.94f),
                    onClick = { onOpen(Route.Transfers) },
                ) { tint -> TransferGlyph(tint, 42.dp) }
                HomeTile(
                    title = "设置",
                    subtitle = "本机 · 传输",
                    accent = Color(0xFF5B5D9E),
                    modifier = Modifier.weight(1f).aspectRatio(0.94f),
                    onClick = { onOpen(Route.Settings) },
                ) { tint -> GearGlyph(tint, 42.dp) }
            }
            Spacer(Modifier.height(20.dp))
            Text(
                text = "局域网直连 · 无需账号 · 文件不经过服务器",
                fontSize = 11.sp,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.fillMaxWidth(),
            )
            Spacer(Modifier.height(20.dp))
        }
    }
}

/** 首页顶部的本机卡片：头像 + 名称 + PeerID。 */
@Composable
private fun SelfCard(self: SelfInfo, online: Int) {
    val context = LocalContext.current
    Card(
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surface),
        elevation = CardDefaults.cardElevation(defaultElevation = 1.dp),
        shape = RoundedCornerShape(16.dp),
    ) {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .clickable(enabled = self.peerId.isNotBlank()) {
                    copyText(context, "FST PeerID", self.peerId, "已复制本机 PeerID")
                }
                .padding(14.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Avatar(b64 = self.avatar, label = self.name.ifBlank { "F" }, size = 46.dp)
            Spacer(Modifier.width(12.dp))
            Column(Modifier.weight(1f)) {
                Text(
                    text = self.name.ifBlank { "本机" },
                    fontWeight = FontWeight.SemiBold,
                    fontSize = 16.sp,
                )
                Spacer(Modifier.height(2.dp))
                Text(
                    text = if (self.peerId.isBlank()) "核心未就绪" else shortPeer(self.peerId),
                    fontSize = 11.sp,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            Text(
                text = "在线 $online 台",
                fontSize = 12.sp,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

/**
 * 首页状态条：一行说明核心是否在跑，点进设置看完整诊断。
 *
 * 详细地址、广播状态与日志都在设置页——首页只保留「是否正常」这一层信息，
 * 避免把排障信息铺满首屏。
 */
@Composable
private fun StatusBanner(coreStatus: CoreStatus, onClick: () -> Unit) {
    val (dot, text) = when (coreStatus.state) {
        CoreState.RUNNING -> Color(0xFF2E7D32) to "核心运行中"
        CoreState.STARTING -> Color(0xFFF9A825) to "核心启动中…"
        CoreState.FAILED -> Color(0xFFC62828) to "核心启动失败"
        CoreState.STOPPED -> Color(0xFF9E9E9E) to "核心未运行"
    }
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .background(MaterialTheme.colorScheme.surfaceVariant, RoundedCornerShape(12.dp))
            .clickable(onClick = onClick)
            .padding(horizontal = 12.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(Modifier.size(8.dp).background(dot, CircleShape))
        Spacer(Modifier.width(8.dp))
        Text(
            text = buildString {
                append(text)
                if (coreStatus.detail.isNotBlank()) append(" · ${coreStatus.detail}")
                if (coreStatus.state == CoreState.FAILED) {
                    append("　").append(coreStatus.logTail.lastOrNull().orEmpty())
                }
            },
            fontSize = 12.sp,
            maxLines = 1,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.weight(1f),
        )
        Text("详情", fontSize = 12.sp, color = MaterialTheme.colorScheme.primary)
    }
}

/**
 * 首页大图标卡片。
 *
 * @param title 标题
 * @param subtitle 状态摘要
 * @param accent 图标与底衬的主题色
 * @param modifier 布局修饰符（用于 weight / aspectRatio）
 * @param onClick 点击回调
 * @param glyph 图标绘制，接收主题色
 */
@Composable
private fun HomeTile(
    title: String,
    subtitle: String,
    accent: Color,
    modifier: Modifier = Modifier,
    onClick: () -> Unit,
    glyph: @Composable (Color) -> Unit,
) {
    Card(
        modifier = modifier.clickable(onClick = onClick),
        shape = RoundedCornerShape(20.dp),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surface),
        elevation = CardDefaults.cardElevation(defaultElevation = 1.dp),
    ) {
        Column(
            modifier = Modifier.fillMaxSize().padding(16.dp),
            verticalArrangement = Arrangement.Center,
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Box(
                modifier = Modifier
                    .size(70.dp)
                    .background(accent.copy(alpha = 0.12f), CircleShape),
                contentAlignment = Alignment.Center,
            ) {
                glyph(accent)
            }
            Spacer(Modifier.height(12.dp))
            Text(title, fontSize = 17.sp, fontWeight = FontWeight.SemiBold)
            Spacer(Modifier.height(3.dp))
            Text(
                text = subtitle,
                fontSize = 12.sp,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}
