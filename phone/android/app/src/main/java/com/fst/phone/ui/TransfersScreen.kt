package com.fst.phone.ui

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
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.fst.phone.data.FstRepository
import com.fst.phone.data.PeerInfo
import com.fst.phone.data.TransferItem

/**
 * 传输页：搜索 + 进度 + 接收确认 + 导出。
 *
 * 参数:
 *  - transfers: List<TransferItem> — 最近 100 条传输记录
 *  - peers: List<PeerInfo> — 用于把 PeerID 换成设备名，搜索时也能按设备名匹配
 *  - onBack: () -> Unit — 返回首页
 *
 * 搜索为什么在客户端做：一次最多 100 条记录，本地过滤比往返 IPC 更快，
 * 也不会因为输入抖动产生大量请求。
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun TransfersScreen(
    transfers: List<TransferItem>,
    peers: List<PeerInfo>,
    onBack: () -> Unit,
) {
    var query by remember { mutableStateOf("") }
    val names = remember(peers) { peers.associate { it.peerId to it.display } }
    val keyword = query.trim()
    val shown = remember(transfers, names, keyword) {
        if (keyword.isBlank()) transfers else transfers.filter { item ->
            item.fileName.contains(keyword, ignoreCase = true) ||
                item.relPath.contains(keyword, ignoreCase = true) ||
                (names[item.peerId] ?: "").contains(keyword, ignoreCase = true) ||
                statusText(item).contains(keyword, ignoreCase = true)
        }
    }
    val running = transfers.count { it.status == "running" || it.status == "accepted" || it.status == "pending" }

    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    Column {
                        Text("传输", fontWeight = FontWeight.SemiBold)
                        Text(
                            text = if (running > 0) "${transfers.size} 条记录 · $running 个进行中" else "${transfers.size} 条记录",
                            fontSize = 11.sp,
                            color = Color.White.copy(alpha = 0.85f),
                        )
                    }
                },
                navigationIcon = {
                    TextButton(onClick = onBack) { Text("返回", color = Color.White) }
                },
                actions = {
                    if (transfers.isNotEmpty()) {
                        TextButton(onClick = { FstRepository.clearTransfers() }) {
                            Text("清理", color = Color.White)
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
        Column(Modifier.padding(padding).fillMaxSize()) {
            OutlinedTextField(
                value = query,
                onValueChange = { query = it },
                modifier = Modifier.fillMaxWidth().padding(horizontal = 14.dp, vertical = 8.dp),
                placeholder = { Text("搜索文件名 / 设备名", fontSize = 13.sp) },
                leadingIcon = { SearchGlyph(MaterialTheme.colorScheme.onSurfaceVariant) },
                trailingIcon = {
                    if (query.isNotEmpty()) {
                        Text(
                            text = "清除",
                            fontSize = 12.sp,
                            color = MaterialTheme.colorScheme.primary,
                            modifier = Modifier.clickable { query = "" }.padding(end = 10.dp),
                        )
                    }
                },
                singleLine = true,
                shape = RoundedCornerShape(14.dp),
                keyboardOptions = KeyboardOptions(imeAction = ImeAction.Search),
                textStyle = MaterialTheme.typography.bodyMedium,
            )
            when {
                transfers.isEmpty() -> EmptyHint("还没有文件传输记录。\n在会话里点「附件」即可发送文件。")
                shown.isEmpty() -> EmptyHint("没有匹配「$keyword」的记录")
                else -> LazyColumn(
                    contentPadding = PaddingValues(horizontal = 12.dp, vertical = 4.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    items(shown, key = { it.id }) { item ->
                        TransferRow(item = item, peerName = names[item.peerId].orEmpty())
                    }
                }
            }
        }
    }
}

/**
 * 一条传输记录。
 *
 * @param item 传输记录
 * @param peerName 对端显示名（未知时显示 PeerID 前 8 位）
 */
@Composable
private fun TransferRow(item: TransferItem, peerName: String) {
    Card(
        shape = RoundedCornerShape(14.dp),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surface),
        elevation = CardDefaults.cardElevation(defaultElevation = 1.dp),
    ) {
        Column(Modifier.padding(14.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    text = if (item.incoming) "↓" else "↑",
                    color = if (item.incoming) Color(0xFF2E7D32) else MaterialTheme.colorScheme.primary,
                    fontWeight = FontWeight.Bold,
                )
                Spacer(Modifier.width(8.dp))
                Text(
                    text = item.fileName,
                    fontWeight = FontWeight.Medium,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f),
                )
                Text(formatSize(item.size), fontSize = 12.sp)
            }
            Spacer(Modifier.height(4.dp))
            Text(
                text = buildString {
                    append(if (item.incoming) "来自 " else "发送给 ")
                    append(peerName.ifBlank { item.peerId.take(8) })
                    if (item.relPath.isNotBlank() && item.relPath != item.fileName) {
                        append(" · ").append(item.relPath)
                    }
                    append(" · ").append(formatTime(item.createdAt))
                },
                fontSize = 11.sp,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
            if (item.status == "running" || item.status == "accepted") {
                Spacer(Modifier.height(8.dp))
                LinearProgressIndicator(
                    progress = { item.progress },
                    modifier = Modifier.fillMaxWidth(),
                )
            }
            Spacer(Modifier.height(6.dp))
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    text = statusText(item),
                    fontSize = 12.sp,
                    color = if (item.status == "failed") {
                        MaterialTheme.colorScheme.error
                    } else {
                        MaterialTheme.colorScheme.onSurfaceVariant
                    },
                    modifier = Modifier.weight(1f),
                )
                if (item.incoming && item.status == "pending") {
                    TextButton(onClick = { FstRepository.acceptOffer(item.batchId) }) { Text("接收") }
                    TextButton(onClick = { FstRepository.rejectOffer(item.batchId) }) { Text("拒绝") }
                }
                if (item.incoming && item.status == "done") {
                    ExportButton(item)
                }
            }
        }
    }
}

/** 导出按钮：把私有目录里的文件复制到系统「下载/FST」。 */
@Composable
private fun ExportButton(item: TransferItem) {
    val context = androidx.compose.ui.platform.LocalContext.current
    TextButton(onClick = { FstRepository.export(context, item) }) { Text("保存到下载") }
}

/** 传输状态的中文描述。 */
internal fun statusText(item: TransferItem): String = when (item.status) {
    "pending" -> "等待确认"
    "accepted" -> "已接受，准备中"
    "running" -> "${(item.progress * 100).toInt()}% · ${formatSize(item.bytesDone)} / ${formatSize(item.size)}"
    "done" -> "已完成"
    "failed" -> "失败：${item.error.ifBlank { "未知原因" }}"
    "canceled", "cancelled" -> "已取消"
    "rejected" -> "已拒绝"
    else -> item.status
}
