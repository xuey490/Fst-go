package com.fst.phone.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
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
import com.fst.phone.data.Conversation
import com.fst.phone.data.PeerInfo

/**
 * 会话页：按最近消息倒序，未读数用主色角标。
 *
 * 参数:
 *  - conversations: List<Conversation> — 会话摘要
 *  - peers: List<PeerInfo> — 用于取显示名与头像
 *  - onBack: () -> Unit — 返回首页
 *  - onOpenChat: (String, String) -> Unit — 打开会话
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ChatsScreen(
    conversations: List<Conversation>,
    peers: List<PeerInfo>,
    onBack: () -> Unit,
    onOpenChat: (String, String) -> Unit,
) {
    val unread = conversations.sumOf { it.unread }
    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    Column {
                        Text("会话", fontWeight = FontWeight.SemiBold)
                        Text(
                            text = if (unread > 0) "${conversations.size} 个会话 · $unread 条未读" else "${conversations.size} 个会话",
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
        if (conversations.isEmpty()) {
            Box(Modifier.padding(padding).fillMaxSize()) {
                EmptyHint("还没有会话。\n到「设备」里选一台设备即可开始聊天。")
            }
            return@Scaffold
        }
        val byId = peers.associateBy { it.peerId }
        LazyColumn(Modifier.padding(padding).fillMaxSize()) {
            items(conversations, key = { it.peerId }) { conv ->
                val peer = byId[conv.peerId]
                val title = peer?.display ?: conv.peerId.take(8)
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .clickable { onOpenChat(conv.peerId, title) }
                        .padding(horizontal = 16.dp, vertical = 12.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Avatar(b64 = peer?.avatar.orEmpty(), label = title, size = 44.dp)
                    Spacer(Modifier.width(12.dp))
                    Column(Modifier.weight(1f)) {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Text(
                                text = title,
                                fontWeight = FontWeight.Medium,
                                fontSize = 15.sp,
                                maxLines = 1,
                                overflow = TextOverflow.Ellipsis,
                                modifier = Modifier.weight(1f, fill = false),
                            )
                            if (peer != null && peer.online) {
                                Spacer(Modifier.width(6.dp))
                                Box(Modifier.width(7.dp).height(7.dp).background(Color(0xFF2E7D32), CircleShape))
                            }
                        }
                        Spacer(Modifier.height(3.dp))
                        Text(
                            text = when {
                                conv.lastRecalled -> "[已撤回]"
                                conv.lastKind == "image" -> "[图片]"
                                conv.lastKind == "file" -> "[文件]"
                                else -> conv.lastContent
                            },
                            fontSize = 13.sp,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            maxLines = 1,
                            overflow = TextOverflow.Ellipsis,
                        )
                    }
                    Spacer(Modifier.width(8.dp))
                    Column(horizontalAlignment = Alignment.End) {
                        Text(
                            text = formatTime(conv.lastTs),
                            fontSize = 11.sp,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                        if (conv.unread > 0) {
                            Spacer(Modifier.height(4.dp))
                            Box(
                                Modifier
                                    .background(MaterialTheme.colorScheme.primary, CircleShape)
                                    .padding(horizontal = 6.dp, vertical = 2.dp),
                            ) {
                                Text("${conv.unread}", color = Color.White, fontSize = 11.sp)
                            }
                        }
                    }
                }
                HorizontalDivider(color = MaterialTheme.colorScheme.surfaceVariant)
            }
        }
    }
}
