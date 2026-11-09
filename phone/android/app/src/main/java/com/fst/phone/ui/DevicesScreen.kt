package com.fst.phone.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
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
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.fst.phone.data.PeerInfo

/**
 * 设备页：在线设备与离线设备分成两段，离线段按设备名去重。
 *
 * 为什么离线要按名字去重：桌面端重装后 libp2p 身份会重新生成（PeerID 变），
 * 旧记录仍留在设备表里，于是同一台电脑会以「同名离线设备」重复出现好几次。
 * 在线设备不合并——那确实是两台正在运行的不同节点（例如两台同名系统机）。
 *
 * 参数:
 *  - peers: List<PeerInfo> — 全部已知设备
 *  - onBack: () -> Unit — 返回首页
 *  - onOpenChat: (String, String) -> Unit — 打开会话（peerId, 标题）
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DevicesScreen(
    peers: List<PeerInfo>,
    onBack: () -> Unit,
    onOpenChat: (String, String) -> Unit,
) {
    val online = peers.filter { it.online }.sortedBy { it.display }
    val offline = dedupeOffline(peers.filterNot { it.online })

    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    Column {
                        Text("设备", fontWeight = FontWeight.SemiBold)
                        Text(
                            text = "在线 ${online.size} 台 · 离线 ${offline.size} 台",
                            fontSize = 11.sp,
                            color = Color.White.copy(alpha = 0.85f),
                        )
                    }
                },
                navigationIcon = {
                    TextButton(onClick = onBack) { Text("返回", color = Color.White) }
                },
                colors = TopAppBarDefaults.topAppBarColors(
                    containerColor = MaterialTheme.colorScheme.primary,
                    titleContentColor = Color.White,
                ),
            )
        },
    ) { padding ->
        if (peers.isEmpty()) {
            Box(Modifier.padding(padding).fillMaxSize()) {
                EmptyHint("正在搜索局域网设备…\n请确认两台设备在同一 Wi-Fi，且电脑端已打开 FST。")
            }
            return@Scaffold
        }
        LazyColumn(
            modifier = Modifier.padding(padding).fillMaxSize(),
            contentPadding = PaddingValues(horizontal = 14.dp, vertical = 10.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            item { SectionHeader("在线设备", "${online.size} 台 · 可直接聊天与传文件") }
            if (online.isEmpty()) {
                item {
                    Text(
                        text = "暂无在线设备，正在持续搜索…",
                        fontSize = 12.sp,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(start = 4.dp, bottom = 6.dp),
                    )
                }
            }
            items(online, key = { it.peerId }) { peer ->
                DeviceRow(peer = peer, onOpenChat = onOpenChat)
            }
            item { Spacer(Modifier.height(6.dp)) }
            item {
                SectionHeader(
                    text = "离线设备",
                    trailing = if (offline.isEmpty()) "暂无" else "${offline.size} 台 · 同名只保留最近一次",
                )
            }
            if (offline.isEmpty()) {
                item {
                    Text(
                        text = "没有离线设备记录",
                        fontSize = 12.sp,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(start = 4.dp, bottom = 6.dp),
                    )
                }
            }
            items(offline, key = { it.peerId }) { peer ->
                DeviceRow(peer = peer, onOpenChat = onOpenChat)
            }
            item { Spacer(Modifier.height(10.dp)) }
        }
    }
}

/**
 * 单台设备行：头像 + 名称 + 平台/地址 + 状态点 + 「进入」。
 *
 * 离线设备也可以进入会话：消息会进入离线队列，对端上线后自动投递。
 *
 * @param peer 设备信息
 * @param onOpenChat 打开会话回调
 */
@Composable
private fun DeviceRow(peer: PeerInfo, onOpenChat: (String, String) -> Unit) {
    Card(
        modifier = Modifier
            .fillMaxWidth()
            .clickable { onOpenChat(peer.peerId, peer.display) },
        shape = RoundedCornerShape(14.dp),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surface),
        elevation = CardDefaults.cardElevation(defaultElevation = 1.dp),
    ) {
        Row(
            modifier = Modifier.padding(horizontal = 14.dp, vertical = 12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Box {
                Avatar(b64 = peer.avatar, label = peer.display, size = 42.dp)
                // 在线状态点压在头像右下角，和头像同一套圆形语言。
                Box(
                    Modifier
                        .align(Alignment.BottomEnd)
                        .size(12.dp)
                        .background(MaterialTheme.colorScheme.surface, CircleShape)
                        .padding(2.dp)
                        .background(
                            if (peer.online) Color(0xFF2E7D32) else Color(0xFFBDBDBD),
                            CircleShape,
                        ),
                )
            }
            Spacer(Modifier.width(12.dp))
            Column(Modifier.weight(1f)) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(
                        text = peer.display,
                        fontWeight = FontWeight.Medium,
                        fontSize = 15.sp,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                        modifier = Modifier.weight(1f, fill = false),
                    )
                    if (peer.trusted) {
                        Spacer(Modifier.width(6.dp))
                        Text(
                            text = "受信",
                            fontSize = 10.sp,
                            color = MaterialTheme.colorScheme.primary,
                            modifier = Modifier
                                .background(
                                    MaterialTheme.colorScheme.primaryContainer.copy(alpha = 0.6f),
                                    RoundedCornerShape(4.dp),
                                )
                                .padding(horizontal = 4.dp, vertical = 1.dp),
                        )
                    }
                }
                Spacer(Modifier.height(2.dp))
                Text(
                    text = buildString {
                        append(peer.platform.ifBlank { "未知平台" })
                        if (peer.ip.isNotBlank()) append(" · ${peer.ip}")
                        if (!peer.online) {
                            append(" · 离线")
                            val seen = formatTime(peer.lastSeen)
                            if (seen.isNotBlank()) append("（$seen）")
                        }
                    },
                    fontSize = 11.sp,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            Text("进入", fontSize = 13.sp, color = MaterialTheme.colorScheme.primary)
        }
    }
}

/**
 * 离线设备去重：同名设备只保留最近在线的那一条。
 *
 * 参数:
 *  - offline: List<PeerInfo> — 离线设备（调用方已过滤）
 *
 * 返回:
 *  - List<PeerInfo> — 按最近在线时间从新到旧、名称唯一
 *
 * 说明：名称为空时 display 会回落到 PeerID 前 8 位，因此不同匿名节点不会被误合并。
 */
internal fun dedupeOffline(offline: List<PeerInfo>): List<PeerInfo> {
    val kept = LinkedHashMap<String, PeerInfo>()
    offline.sortedByDescending { it.lastSeen }.forEach { peer ->
        val key = peer.display.trim().lowercase()
        if (!kept.containsKey(key)) kept[key] = peer
    }
    return kept.values.toList()
}
