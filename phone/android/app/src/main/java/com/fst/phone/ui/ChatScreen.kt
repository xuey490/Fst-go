package com.fst.phone.ui

import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
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
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
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
import com.fst.phone.data.ChatMessage
import com.fst.phone.data.FstRepository

/**
 * 单聊界面。
 *
 * 参数:
 *  - peerId: String — 对方 PeerID
 *  - title: String — 顶栏标题（对方名称）
 *  - onBack: () -> Unit — 返回回调
 *
 * 行为：进入即加载最近 200 条并标记已读；点「附件」调用系统文件选择器，
 * 选中后由仓库层复制到私有发件箱再发起传输。
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ChatScreen(peerId: String, title: String, onBack: () -> Unit) {
    val context = LocalContext.current
    val messages by FstRepository.messages.collectAsStateWithLifecycle()
    var input by remember { mutableStateOf("") }
    val listState = rememberLazyListState()

    val picker = rememberLauncherForActivityResult(
        ActivityResultContracts.OpenMultipleDocuments(),
    ) { uris ->
        if (uris.isNotEmpty()) FstRepository.sendDocuments(context, peerId, uris)
    }

    LaunchedEffect(peerId) { FstRepository.openChat(peerId) }
    LaunchedEffect(messages.size) {
        if (messages.isNotEmpty()) listState.animateScrollToItem(messages.lastIndex)
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    Column {
                        Text(title, fontWeight = FontWeight.SemiBold)
                        Text(
                            text = shortPeer(peerId),
                            fontSize = 11.sp,
                            color = Color.White.copy(alpha = 0.85f),
                            modifier = Modifier.clickable {
                                copyText(context, "FST PeerID", peerId, "已复制对方 PeerID")
                            },
                        )
                    }
                },
                navigationIcon = {
                    TextButton(onClick = onBack) {
                        Text("返回", color = Color.White)
                    }
                },
                actions = {
                    TextButton(onClick = { picker.launch(arrayOf("*/*")) }) {
                        Text("附件", color = Color.White)
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
            Modifier
                .padding(padding)
                .fillMaxSize()
                .imePadding(),
        ) {
            if (messages.isEmpty()) {
                Box(Modifier.weight(1f).fillMaxWidth(), contentAlignment = Alignment.Center) {
                    Text(
                        "还没有消息，说点什么吧",
                        fontSize = 13.sp,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            } else {
                LazyColumn(
                    state = listState,
                    modifier = Modifier.weight(1f).fillMaxWidth(),
                    contentPadding = PaddingValues(horizontal = 12.dp, vertical = 10.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    items(messages, key = { it.id.ifBlank { it.ts.toString() } }) { m ->
                        Bubble(m)
                    }
                }
            }
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .background(MaterialTheme.colorScheme.surface)
                    .navigationBarsPadding()
                    .padding(horizontal = 8.dp, vertical = 6.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                OutlinedTextField(
                    value = input,
                    onValueChange = { input = it },
                    modifier = Modifier.weight(1f),
                    placeholder = { Text("输入消息", fontSize = 14.sp) },
                    maxLines = 4,
                    shape = RoundedCornerShape(20.dp),
                )
                Spacer(Modifier.padding(4.dp))
                TextButton(
                    onClick = {
                        val text = input
                        input = ""
                        FstRepository.sendText(peerId, text)
                    },
                    enabled = input.isNotBlank(),
                ) { Text("发送") }
            }
        }
    }
}

/** 单条消息气泡：自己发的靠右（主色），收到的靠左（浅灰）。 */
@Composable
private fun Bubble(m: ChatMessage) {
    val mine = m.mine
    Row(
        Modifier.fillMaxWidth(),
        horizontalArrangement = if (mine) Arrangement.End else Arrangement.Start,
    ) {
        Column(
            modifier = Modifier
                .widthIn(max = 280.dp)
                .background(
                    color = if (mine) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.surfaceVariant,
                    shape = RoundedCornerShape(
                        topStart = 14.dp,
                        topEnd = 14.dp,
                        bottomStart = if (mine) 14.dp else 3.dp,
                        bottomEnd = if (mine) 3.dp else 14.dp,
                    ),
                )
                .padding(horizontal = 12.dp, vertical = 8.dp),
        ) {
            Text(
                text = if (m.recalled) "[已撤回]" else messageSummary(m),
                color = if (mine) Color.White else MaterialTheme.colorScheme.onSurface,
                fontSize = 14.sp,
            )
            Spacer(Modifier.height(2.dp))
            Text(
                text = buildString {
                    append(formatTime(m.ts))
                    if (mine && m.status == "sending") append(" · 发送中")
                    if (mine && m.status == "failed") append(" · 失败")
                    if (mine && m.status == "queued") append(" · 待投递")
                },
                fontSize = 10.sp,
                color = if (mine) Color.White.copy(alpha = 0.7f) else MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}
