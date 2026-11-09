package com.fst.phone.ui

import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
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
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Switch
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
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.foundation.text.KeyboardOptions
import com.fst.phone.data.CoreState
import com.fst.phone.data.CoreStatus
import com.fst.phone.data.FstRepository
import com.fst.phone.data.PeerInfo
import com.fst.phone.data.SelfInfo
import com.fst.phone.data.SettingsInfo

/**
 * 设置页：与桌面端可对齐的部分全部做进来。
 *
 * 覆盖项：设备昵称、头像、PeerID / 监听地址、核心状态与日志、并发文件数、
 * 传输限速、发送前预校验、自动接收、受信设备、清理传输记录、重启核心。
 *
 * 桌面端独有、移动端没有对应实现的项（开机自启、关闭按钮行为、只允许单实例、
 * 跨网络中继与代理）在页尾明确说明，避免用户找不到入口时以为是缺漏。
 *
 * 参数:
 *  - self: SelfInfo — 本机信息（PeerID、地址、头像）
 *  - settings: SettingsInfo — 核心里的设置快照
 *  - peers: List<PeerInfo> — 用于受信设备开关
 *  - coreStatus: CoreStatus — 核心生命周期与日志
 *  - nsdStatus: String — DNS-SD 广播状态
 *  - onBack: () -> Unit — 返回首页
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SettingsScreen(
    self: SelfInfo,
    settings: SettingsInfo,
    peers: List<PeerInfo>,
    coreStatus: CoreStatus,
    nsdStatus: String,
    onBack: () -> Unit,
) {
    val context = LocalContext.current
    var nameInput by remember { mutableStateOf(settings.name) }
    var concurrency by remember { mutableStateOf(settings.concurrency) }
    var limitInput by remember { mutableStateOf(settings.rateLimitKbps.toString()) }
    var confirmClear by remember { mutableStateOf(false) }

    // 核心里的值变化（保存成功、外部改动）时同步到输入框；正在输入时不打断。
    LaunchedEffect(settings.name) {
        if (settings.name != nameInput) nameInput = settings.name
    }
    LaunchedEffect(settings.concurrency) { concurrency = settings.concurrency }
    LaunchedEffect(settings.rateLimitKbps) { limitInput = settings.rateLimitKbps.toString() }

    val avatarPicker = rememberLauncherForActivityResult(
        ActivityResultContracts.GetContent(),
    ) { uri -> if (uri != null) FstRepository.setAvatar(context, uri) }

    /** 把当前输入框里的值一并提交；开关也走这里，避免两次保存互相覆盖。 */
    fun persist(prehash: Boolean = settings.prehash, autoAccept: Boolean = settings.autoAccept) {
        FstRepository.saveSettings(
            name = nameInput,
            concurrency = concurrency,
            rateLimitKbps = limitInput.trim().toIntOrNull()?.coerceIn(0, 1 shl 20) ?: 0,
            prehash = prehash,
            autoAccept = autoAccept,
        )
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    Column {
                        Text("设置", fontWeight = FontWeight.SemiBold)
                        Text(
                            text = "本机 · 传输 · 维护",
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
        Column(
            modifier = Modifier
                .padding(padding)
                .fillMaxSize()
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 14.dp),
        ) {
            Spacer(Modifier.height(10.dp))

            SectionHeader("本机")
            SettingCard {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Box(Modifier.clickable { avatarPicker.launch("image/*") }) {
                        Avatar(b64 = self.avatar, label = self.name.ifBlank { "F" }, size = 58.dp)
                    }
                    Spacer(Modifier.width(14.dp))
                    Column(Modifier.weight(1f)) {
                        Text("头像", fontSize = 14.sp)
                        Spacer(Modifier.height(2.dp))
                        Text(
                            text = "PNG/JPEG，随 HELLO 同步给已连接设备",
                            fontSize = 11.sp,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                    TextButton(onClick = { avatarPicker.launch("image/*") }) { Text("选择") }
                    if (self.avatar.isNotBlank()) {
                        TextButton(onClick = { FstRepository.clearAvatar() }) { Text("清除") }
                    }
                }
                Spacer(Modifier.height(6.dp))
                HorizontalDivider(color = MaterialTheme.colorScheme.surfaceVariant)

                Spacer(Modifier.height(10.dp))
                Text("设备昵称", fontSize = 14.sp)
                Spacer(Modifier.height(6.dp))
                Row(verticalAlignment = Alignment.CenterVertically) {
                    OutlinedTextField(
                        value = nameInput,
                        onValueChange = { if (it.length <= 40) nameInput = it },
                        modifier = Modifier.weight(1f),
                        singleLine = true,
                        placeholder = { Text("例如 我的手机", fontSize = 13.sp) },
                        shape = RoundedCornerShape(12.dp),
                    )
                    Spacer(Modifier.width(8.dp))
                    Button(
                        onClick = { persist() },
                        enabled = nameInput.isNotBlank() && nameInput.trim() != settings.name,
                    ) { Text("保存") }
                }

                Spacer(Modifier.height(10.dp))
                HorizontalDivider(color = MaterialTheme.colorScheme.surfaceVariant)
                InfoLine(
                    label = "PeerID",
                    value = self.peerId.ifBlank { "核心未就绪" },
                    trailing = if (self.peerId.isBlank()) "" else "复制",
                    onClick = if (self.peerId.isBlank()) {
                        null
                    } else {
                        { copyText(context, "FST PeerID", self.peerId, "已复制本机 PeerID") }
                    },
                )
                val addrs = routableAddrs(self)
                InfoLine(
                    label = "监听地址",
                    value = if (addrs.isEmpty()) "未取得局域网地址（请确认已连接 Wi-Fi）" else addrs.joinToString("\n"),
                    trailing = if (addrs.isEmpty()) "" else "复制",
                    onClick = if (addrs.isEmpty()) {
                        null
                    } else {
                        { copyText(context, "FST 地址", addrs.joinToString(" "), "已复制本机地址") }
                    },
                )
                InfoLine(label = "广播", value = nsdStatus)
                InfoLine(
                    label = "版本",
                    value = "应用 ${settings.version.ifBlank { "—" }} · 协议 ${settings.protocolVersion.ifBlank { "—" }}",
                )
            }

            CoreStatusCard(coreStatus = coreStatus, nsdStatus = nsdStatus)

            Spacer(Modifier.height(6.dp))
            SectionHeader("传输")
            SettingCard {
                // 并发文件数：与桌面端一致，1–8
                Row(verticalAlignment = Alignment.CenterVertically) {
                    SettingLabel("并发文件数", "同时传输的文件数量，1–8")
                    StepperValue(
                        value = concurrency,
                        onChange = { concurrency = it.coerceIn(1, 8) },
                    )
                }
                Spacer(Modifier.height(10.dp))
                HorizontalDivider(color = MaterialTheme.colorScheme.surfaceVariant)
                Spacer(Modifier.height(10.dp))
                Text("传输限速", fontSize = 14.sp)
                Spacer(Modifier.height(6.dp))
                OutlinedTextField(
                    value = limitInput,
                    onValueChange = { v -> limitInput = v.filter { it.isDigit() }.take(7) },
                    modifier = Modifier.fillMaxWidth(),
                    singleLine = true,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                    supportingText = { Text("KB/s，0 表示不限速（收发合计，立即生效）", fontSize = 11.sp) },
                    shape = RoundedCornerShape(12.dp),
                )
                SwitchRow(
                    title = "发送前预校验",
                    subtitle = "先算整文件 SHA-256，接收端已有相同文件时秒跳过（大文件会多读一遍）",
                    checked = settings.prehash,
                    onCheckedChange = { checked -> persist(prehash = checked) },
                )
                SwitchRow(
                    title = "自动接收",
                    subtitle = "对所有设备跳过接收确认弹窗（不推荐；只想信任单台设备请在下方设置）",
                    checked = settings.autoAccept,
                    onCheckedChange = { checked -> persist(autoAccept = checked) },
                )
                Spacer(Modifier.height(6.dp))
                Button(onClick = { persist() }, modifier = Modifier.fillMaxWidth()) {
                    Text("保存传输设置")
                }
            }

            Spacer(Modifier.height(6.dp))
            SectionHeader("受信设备", "受信设备的文件会被自动接收")
            SettingCard {
                if (peers.isEmpty()) {
                    Text(
                        text = "还没有已知设备。发现过的设备会出现在这里。",
                        fontSize = 12.sp,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                } else {
                    peers.sortedByDescending { it.trusted }.forEachIndexed { index, peer ->
                        if (index > 0) {
                            HorizontalDivider(color = MaterialTheme.colorScheme.surfaceVariant)
                        }
                        Row(
                            modifier = Modifier
                                .fillMaxWidth()
                                .padding(vertical = 8.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            Avatar(b64 = peer.avatar, label = peer.display, size = 34.dp)
                            Spacer(Modifier.width(10.dp))
                            Column(Modifier.weight(1f)) {
                                Text(peer.display, fontSize = 14.sp, maxLines = 1, overflow = TextOverflow.Ellipsis)
                                Text(
                                    text = buildString {
                                        append(if (peer.online) "在线" else "离线")
                                        if (peer.ip.isNotBlank()) append(" · ").append(peer.ip)
                                    },
                                    fontSize = 11.sp,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                            }
                            Switch(
                                checked = peer.trusted,
                                onCheckedChange = { checked -> FstRepository.setTrusted(peer.peerId, checked) },
                            )
                        }
                    }
                }
            }

            Spacer(Modifier.height(6.dp))
            SectionHeader("维护")
            SettingCard {
                ActionRow(
                    title = "清理传输记录",
                    subtitle = "删除已完成/失败/取消的记录，文件本身不删",
                    onClick = { confirmClear = true },
                )
                HorizontalDivider(color = MaterialTheme.colorScheme.surfaceVariant)
                ActionRow(
                    title = "重启核心",
                    subtitle = "重新拉起 Go 核心并重新广播（异常时使用）",
                    onClick = { FstRepository.restart(context) },
                )
                HorizontalDivider(color = MaterialTheme.colorScheme.surfaceVariant)
                InfoLine(label = "下载目录", value = settings.downloadDir.ifBlank { "—" })
                InfoLine(label = "数据目录", value = settings.dataDir.ifBlank { self.downloadDir })
            }

            Spacer(Modifier.height(6.dp))
            SectionHeader("关于")
            SettingCard {
                Text(
                    text = "FST 是去中心化的局域网直连工具：没有账号、没有中心服务器，设备之间用 " +
                        "libp2p 直连，聊天与文件只存在各自本机。",
                    fontSize = 12.sp,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Spacer(Modifier.height(10.dp))
                Text(
                    text = "桌面端专属选项（开机自启、关闭按钮行为、只允许单实例、跨网络中继与代理）" +
                        "依赖 Windows 外壳，移动端没有对应实现，因此不在此页出现。",
                    fontSize = 12.sp,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            Spacer(Modifier.height(24.dp))
        }
    }

    if (confirmClear) {
        AlertDialog(
            onDismissRequest = { confirmClear = false },
            title = { Text("清理传输记录") },
            text = { Text("将删除已结束的传输记录（文件保留在下载目录）。聊天里引用的记录会被自动跳过。") },
            confirmButton = {
                TextButton(onClick = {
                    confirmClear = false
                    FstRepository.clearTransfers()
                }) { Text("清理") }
            },
            dismissButton = {
                TextButton(onClick = { confirmClear = false }) { Text("取消") }
            },
        )
    }
}

/** 设置页统一的卡片容器：白底、圆角、内边距。 */
@Composable
private fun SettingCard(content: @Composable () -> Unit) {
    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(16.dp),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surface),
        elevation = CardDefaults.cardElevation(defaultElevation = 1.dp),
    ) {
        Column(Modifier.padding(horizontal = 14.dp, vertical = 12.dp)) { content() }
    }
}

/** 一行「标签 — 值」，值可点击复制。 */
@Composable
private fun InfoLine(label: String, value: String, trailing: String = "", onClick: (() -> Unit)? = null) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .then(if (onClick != null) Modifier.clickable(onClick = onClick) else Modifier)
            .padding(vertical = 8.dp),
        verticalAlignment = Alignment.Top,
    ) {
        Text(
            text = label,
            fontSize = 13.sp,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.width(72.dp),
        )
        Text(
            text = value,
            fontSize = 12.sp,
            color = MaterialTheme.colorScheme.onSurface,
            modifier = Modifier.weight(1f),
        )
        if (trailing.isNotBlank()) {
            Spacer(Modifier.width(6.dp))
            Text(trailing, fontSize = 12.sp, color = MaterialTheme.colorScheme.primary)
        }
    }
}

/** 带说明文本与开关的一行。 */
@Composable
private fun SwitchRow(
    title: String,
    subtitle: String,
    checked: Boolean,
    onCheckedChange: (Boolean) -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(top = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        SettingLabel(title, subtitle)
        Spacer(Modifier.width(10.dp))
        Switch(checked = checked, onCheckedChange = onCheckedChange)
    }
}

/** 可点击的动作行。 */
@Composable
private fun ActionRow(title: String, subtitle: String, onClick: () -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .clickable(onClick = onClick)
            .padding(vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        SettingLabel(title, subtitle)
        Text("›", fontSize = 18.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

/** 数值步进器：−  3  ＋。 */
@Composable
private fun StepperValue(value: Int, onChange: (Int) -> Unit) {
    Row(
        horizontalArrangement = Arrangement.spacedBy(2.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        TextButton(onClick = { onChange(value - 1) }, enabled = value > 1) { Text("−", fontSize = 18.sp) }
        Text("$value", fontSize = 15.sp, fontWeight = FontWeight.Medium, modifier = Modifier.width(24.dp))
        TextButton(onClick = { onChange(value + 1) }, enabled = value < 8) { Text("＋", fontSize = 15.sp) }
    }
}

/**
 * 核心状态卡片：把「启动中 / 运行中 / 失败原因」与日志尾部摊开。
 *
 * 核心是独立进程，失败时界面只会显示「正在搜索设备…」，与「正常运行但局域网
 * 无对端」同形；这里把端口、PeerID、广播状态和失败原文都放出来，用户不接
 * USB 也能报出原因。
 *
 * @param coreStatus 核心生命周期状态
 * @param nsdStatus DNS-SD 广播状态
 */
@Composable
private fun CoreStatusCard(coreStatus: CoreStatus, nsdStatus: String) {
    val context = LocalContext.current
    var expanded by remember { mutableStateOf(false) }
    val (dotColor, title) = when (coreStatus.state) {
        CoreState.RUNNING -> Color(0xFF2E7D32) to "核心运行中"
        CoreState.STARTING -> Color(0xFFF9A825) to "核心启动中…"
        CoreState.FAILED -> Color(0xFFC62828) to "核心启动失败"
        CoreState.STOPPED -> Color(0xFF9E9E9E) to "核心未运行"
    }
    Spacer(Modifier.height(8.dp))
    SettingCard {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Box(Modifier.size(8.dp).background(dotColor, CircleShape))
            Spacer(Modifier.width(8.dp))
            Text(title, fontSize = 13.sp, fontWeight = FontWeight.Medium, modifier = Modifier.weight(1f))
            if (coreStatus.state == CoreState.FAILED || coreStatus.state == CoreState.STOPPED) {
                TextButton(onClick = { FstRepository.restart(context) }) { Text("重试", fontSize = 12.sp) }
            }
            if (coreStatus.logTail.isNotEmpty()) {
                TextButton(onClick = { expanded = !expanded }) {
                    Text(if (expanded) "收起" else "日志", fontSize = 12.sp)
                }
            }
        }
        if (coreStatus.detail.isNotBlank()) {
            Text(
                text = coreStatus.detail,
                fontSize = 11.sp,
                color = if (coreStatus.state == CoreState.FAILED) Color(0xFFC62828) else MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        if (coreStatus.state == CoreState.RUNNING) {
            Text(
                text = "广播：$nsdStatus",
                fontSize = 11.sp,
                color = if (nsdStatus.startsWith("已注册")) {
                    MaterialTheme.colorScheme.onSurfaceVariant
                } else {
                    Color(0xFFC62828)
                },
            )
        }
        if (expanded) {
            Column(Modifier.fillMaxWidth().padding(top = 6.dp)) {
                coreStatus.logTail.takeLast(40).forEach { line ->
                    Text(line, fontSize = 9.sp, maxLines = 3, overflow = TextOverflow.Ellipsis)
                }
            }
        }
    }
}
