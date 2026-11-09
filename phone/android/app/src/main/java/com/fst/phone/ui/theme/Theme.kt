package com.fst.phone.ui.theme

import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color

/** FST 品牌色：深绿主色 + 浅绿容器，与桌面端的绿色基调一致。 */
private val FstColors = lightColorScheme(
    primary = Color(0xFF1B5E4B),
    onPrimary = Color(0xFFFFFFFF),
    primaryContainer = Color(0xFFB7E4D1),
    onPrimaryContainer = Color(0xFF002014),
    secondary = Color(0xFF4E635A),
    onSecondary = Color(0xFFFFFFFF),
    background = Color(0xFFFBFDF9),
    onBackground = Color(0xFF191C1A),
    surface = Color(0xFFFFFFFF),
    onSurface = Color(0xFF191C1A),
    surfaceVariant = Color(0xFFDBE5DE),
    onSurfaceVariant = Color(0xFF404944),
    outline = Color(0xFF707974),
    error = Color(0xFFBA1A1A),
)

/**
 * 应用主题。
 *
 * 参数:
 *  - content: @Composable () -> Unit — 主题作用域内的界面
 */
@Composable
fun FstTheme(content: @Composable () -> Unit) {
    MaterialTheme(colorScheme = FstColors, content = content)
}
