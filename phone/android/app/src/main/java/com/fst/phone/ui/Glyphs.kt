package com.fst.phone.ui

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.size
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.StrokeJoin
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp

/**
 * FST 首页图标集：设备 / 会话 / 传输 / 设置。
 *
 * 为什么自己画而不是引图标库：图标库（material-icons-extended）会让 APK 增大
 * 十几 MB，且本仓库的 Compose 依赖是裁剪过的，只有几个基础图标可用。这四个
 * 图形全部基于同一套线宽与圆角端点，风格统一，随主题色变化。
 */

/** 统一线宽比例（相对图形边长）。 */
private const val STROKE_RATIO = 0.075f

/** 构造统一的描边样式。 */
private fun outline(widthPx: Float) = Stroke(
    width = widthPx,
    cap = StrokeCap.Round,
    join = StrokeJoin.Round,
)

/**
 * 设备图标：显示器 + 支架。
 *
 * @param tint 线条颜色
 * @param size 图标边长
 */
@Composable
fun DeviceGlyph(tint: Color, size: Dp = 40.dp) {
    Canvas(Modifier.size(size)) {
        val w = this.size.width
        val h = this.size.height
        val sw = w * STROKE_RATIO
        val stroke = outline(sw)
        drawRoundRect(
            color = tint,
            topLeft = Offset(w * 0.10f, h * 0.14f),
            size = Size(w * 0.80f, h * 0.54f),
            cornerRadius = CornerRadius(w * 0.12f),
            style = stroke,
        )
        drawCircle(tint, radius = w * 0.045f, center = Offset(w * 0.50f, h * 0.41f))
        drawLine(tint, Offset(w * 0.50f, h * 0.68f), Offset(w * 0.50f, h * 0.80f), sw, StrokeCap.Round)
        drawLine(tint, Offset(w * 0.28f, h * 0.83f), Offset(w * 0.72f, h * 0.83f), sw, StrokeCap.Round)
    }
}

/**
 * 会话图标：对话气泡 + 尾巴。
 *
 * @param tint 线条颜色
 * @param size 图标边长
 */
@Composable
fun ChatGlyph(tint: Color, size: Dp = 40.dp) {
    Canvas(Modifier.size(size)) {
        val w = this.size.width
        val h = this.size.height
        val sw = w * STROKE_RATIO
        val stroke = outline(sw)
        drawRoundRect(
            color = tint,
            topLeft = Offset(w * 0.09f, h * 0.13f),
            size = Size(w * 0.82f, h * 0.56f),
            cornerRadius = CornerRadius(w * 0.20f),
            style = stroke,
        )
        // 尾巴与气泡底边同为 tint、同为圆头，视觉上连成一体。
        val tail = Path().apply {
            moveTo(w * 0.30f, h * 0.66f)
            lineTo(w * 0.30f, h * 0.90f)
            lineTo(w * 0.52f, h * 0.69f)
        }
        drawPath(tail, tint, style = stroke)
        drawCircle(tint, radius = w * 0.035f, center = Offset(w * 0.36f, h * 0.41f))
        drawCircle(tint, radius = w * 0.035f, center = Offset(w * 0.50f, h * 0.41f))
        drawCircle(tint, radius = w * 0.035f, center = Offset(w * 0.64f, h * 0.41f))
    }
}

/**
 * 传输图标：一上一下两支箭头。
 *
 * @param tint 线条颜色
 * @param size 图标边长
 */
@Composable
fun TransferGlyph(tint: Color, size: Dp = 40.dp) {
    Canvas(Modifier.size(size)) {
        val w = this.size.width
        val h = this.size.height
        val sw = w * STROKE_RATIO
        val cap = StrokeCap.Round
        // 上箭头
        drawLine(tint, Offset(w * 0.34f, h * 0.84f), Offset(w * 0.34f, h * 0.20f), sw, cap)
        drawLine(tint, Offset(w * 0.19f, h * 0.36f), Offset(w * 0.34f, h * 0.19f), sw, cap)
        drawLine(tint, Offset(w * 0.49f, h * 0.36f), Offset(w * 0.34f, h * 0.19f), sw, cap)
        // 下箭头
        drawLine(tint, Offset(w * 0.66f, h * 0.16f), Offset(w * 0.66f, h * 0.80f), sw, cap)
        drawLine(tint, Offset(w * 0.51f, h * 0.64f), Offset(w * 0.66f, h * 0.81f), sw, cap)
        drawLine(tint, Offset(w * 0.81f, h * 0.64f), Offset(w * 0.66f, h * 0.81f), sw, cap)
    }
}

/**
 * 搜索图标：放大镜（圆 + 手柄）。
 *
 * @param tint 线条颜色
 * @param size 图标边长
 */
@Composable
fun SearchGlyph(tint: Color, size: Dp = 18.dp) {
    Canvas(Modifier.size(size)) {
        val w = this.size.width
        val h = this.size.height
        val sw = w * 0.10f
        drawCircle(
            color = tint,
            radius = w * 0.28f,
            center = Offset(w * 0.42f, h * 0.42f),
            style = Stroke(width = sw, cap = StrokeCap.Round),
        )
        drawLine(
            color = tint,
            start = Offset(w * 0.63f, h * 0.63f),
            end = Offset(w * 0.85f, h * 0.85f),
            strokeWidth = sw,
            cap = StrokeCap.Round,
        )
    }
}

/**
 * 设置图标：齿轮（圆环 + 八颗齿）。
 *
 * @param tint 线条颜色
 * @param size 图标边长
 */
@Composable
fun GearGlyph(tint: Color, size: Dp = 40.dp) {
    Canvas(Modifier.size(size)) {
        val w = this.size.width
        val h = this.size.height
        val sw = w * STROKE_RATIO
        val cx = w / 2f
        val cy = h / 2f
        drawCircle(tint, radius = w * 0.22f, center = Offset(cx, cy), style = outline(sw))
        // 八颗齿：用短粗线段代替齿形路径，缩放后仍然清晰。
        repeat(8) { i ->
            val rad = Math.toRadians(i * 45.0)
            val cos = kotlin.math.cos(rad).toFloat()
            val sin = kotlin.math.sin(rad).toFloat()
            drawLine(
                color = tint,
                start = Offset(cx + cos * w * 0.30f, cy + sin * h * 0.30f),
                end = Offset(cx + cos * w * 0.40f, cy + sin * h * 0.40f),
                strokeWidth = sw * 1.35f,
                cap = StrokeCap.Round,
            )
        }
    }
}
