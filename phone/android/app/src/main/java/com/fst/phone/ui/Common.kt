package com.fst.phone.ui

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.graphics.BitmapFactory
import android.util.Base64
import android.widget.Toast
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.RowScope
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.fst.phone.data.ChatMessage
import com.fst.phone.data.SelfInfo
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * 圆形头像：有 base64 图片就显示图片，否则显示名称首字符。
 *
 * @param b64 头像的 base64（PNG/JPEG）；空串表示无头像
 * @param label 名称，用于取首字符与无障碍描述
 * @param size 直径
 *
 * 说明：图片在 [remember] 里只解码一次——设备列表每次事件刷新都会重组，
 * 每帧解码 base64 会明显掉帧。
 */
@Composable
fun Avatar(b64: String, label: String, size: Dp = 44.dp) {
    val bitmap = remember(b64) { decodeAvatar(b64) }
    Box(
        modifier = Modifier
            .size(size)
            .clip(CircleShape)
            .background(MaterialTheme.colorScheme.primaryContainer),
        contentAlignment = Alignment.Center,
    ) {
        if (bitmap != null) {
            Image(
                bitmap = bitmap,
                contentDescription = label,
                contentScale = ContentScale.Crop,
                modifier = Modifier.fillMaxSize(),
            )
        } else {
            Text(
                text = label.trim().take(1).uppercase().ifBlank { "?" },
                fontSize = (size.value * 0.42f).sp,
                fontWeight = FontWeight.SemiBold,
                color = MaterialTheme.colorScheme.onPrimaryContainer,
            )
        }
    }
}

/** 把 base64 头像解码成位图；空串或损坏数据返回 null。 */
private fun decodeAvatar(b64: String): ImageBitmap? {
    if (b64.isBlank()) return null
    return runCatching {
        val bytes = Base64.decode(b64, Base64.DEFAULT)
        BitmapFactory.decodeByteArray(bytes, 0, bytes.size)?.asImageBitmap()
    }.getOrNull()
}

/**
 * 分组标题：小字、加粗、带可选计数。
 *
 * @param text 标题文字
 * @param trailing 右侧补充说明（如「3 台」）
 */
@Composable
fun SectionHeader(text: String, trailing: String = "") {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(start = 4.dp, end = 4.dp, top = 6.dp, bottom = 6.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(
            text = text,
            fontSize = 12.sp,
            fontWeight = FontWeight.SemiBold,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        if (trailing.isNotBlank()) {
            Spacer(Modifier.width(6.dp))
            Text(text = trailing, fontSize = 11.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

/**
 * 设置项标签 + 说明的两行文本，只能用在 Row 里（占用剩余宽度）。
 *
 * @param title 主标题
 * @param subtitle 说明文字，空则只显示标题
 */
@Composable
fun RowScope.SettingLabel(title: String, subtitle: String = "") {
    Column(Modifier.weight(1f)) {
        Text(title, fontSize = 14.sp)
        if (subtitle.isNotBlank()) {
            Spacer(Modifier.height(2.dp))
            Text(
                text = subtitle,
                fontSize = 11.sp,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

/** 空状态提示（居中、灰色小字）。 */
@Composable
fun EmptyHint(text: String) {
    Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
        Text(
            text = text,
            fontSize = 13.sp,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.padding(32.dp),
        )
    }
}

/** 字节数转可读大小。 */
internal fun formatSize(bytes: Long): String {
    if (bytes < 1024) return "$bytes B"
    val kb = bytes / 1024.0
    if (kb < 1024) return String.format(Locale.US, "%.1f KB", kb)
    val mb = kb / 1024.0
    if (mb < 1024) return String.format(Locale.US, "%.1f MB", mb)
    return String.format(Locale.US, "%.2f GB", mb / 1024.0)
}

/** 时间戳（毫秒）转「今天 HH:mm」或「MM-dd」。 */
internal fun formatTime(ts: Long): String {
    if (ts <= 0) return ""
    val now = System.currentTimeMillis()
    val sameDay = (now - ts) < 24 * 60 * 60 * 1000L
    val pattern = if (sameDay) "HH:mm" else "MM-dd"
    return SimpleDateFormat(pattern, Locale.getDefault()).format(Date(ts))
}

/** 消息气泡里显示的单行摘要（文件类消息用附件名代替正文）。 */
internal fun messageSummary(m: ChatMessage): String = when {
    m.recalled -> "[已撤回]"
    m.kind == "image" -> "[图片]"
    m.kind == "file" -> m.content.ifBlank { "[文件]" }
    else -> m.content
}

/**
 * 过滤出对端可连的本机地址（去掉回环与未指定地址）。
 *
 * @param self 本机信息，取自 `/self` 的 addrs
 * @return 可路由地址列表；核心还没拿到局域网 IPv4 时为空
 */
internal fun routableAddrs(self: SelfInfo): List<String> = self.addrs.filterNot { addr ->
    addr.contains("/ip4/127.") || addr.contains("/ip4/0.0.0.0") || addr.contains("/ip6/::1")
}

/**
 * PeerID 的短显示形式：前 10 位加省略号。
 *
 * @param peerId 完整 PeerID；空串原样返回
 * @return 形如 `12D3KooWQU…` 的字符串
 */
internal fun shortPeer(peerId: String): String =
    if (peerId.length <= 12) peerId else peerId.take(10) + "…"

/**
 * 复制文本到剪贴板并给出提示。
 *
 * @param context 用于取剪贴板服务
 * @param label 剪贴板条目的标签
 * @param text 要复制的完整文本
 * @param toast 复制成功后的提示语
 */
internal fun copyText(context: Context, label: String, text: String, toast: String) {
    runCatching {
        val cm = context.getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager
        cm.setPrimaryClip(ClipData.newPlainText(label, text))
        Toast.makeText(context, toast, Toast.LENGTH_SHORT).show()
    }
}
