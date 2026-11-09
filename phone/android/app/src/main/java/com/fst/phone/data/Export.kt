package com.fst.phone.data

import android.content.ContentValues
import android.content.Context
import android.os.Build
import android.os.Environment
import android.provider.MediaStore
import java.io.File

/**
 * Export 把私有目录里的接收文件导出到用户可见的「下载/FST」。
 *
 * 两条路径：
 *  - Android 10+：走 MediaStore.Downloads，无需任何存储权限。
 *  - Android 8/9：直接写公共下载目录，需要 WRITE_EXTERNAL_STORAGE。
 */
object Export {

    /**
     * 复制一个文件到系统下载目录。
     *
     * 参数:
     *  - context: Context — 应用上下文
     *  - src: File — 源文件（通常是私有下载目录里的 local_path）
     *  - name: String — 目标文件名
     *
     * 返回:
     *  - Unit
     *
     * 异常:
     *  - IllegalStateException — 源文件不存在或系统拒绝创建条目
     *  - SecurityException — Android 9 及以下未授予写存储权限
     */
    fun copyToDownloads(context: Context, src: File, name: String) {
        if (!src.isFile) throw IllegalStateException("源文件不存在：${src.name}")
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            val resolver = context.contentResolver
            val values = ContentValues().apply {
                put(MediaStore.Downloads.DISPLAY_NAME, name)
                put(MediaStore.Downloads.MIME_TYPE, mimeOf(name))
                put(MediaStore.Downloads.RELATIVE_PATH, "${Environment.DIRECTORY_DOWNLOADS}/FST")
                put(MediaStore.Downloads.IS_PENDING, 1)
            }
            val uri = resolver.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, values)
                ?: throw IllegalStateException("系统未允许创建下载条目")
            try {
                resolver.openOutputStream(uri)?.use { out ->
                    src.inputStream().use { input -> input.copyTo(out, 64 * 1024) }
                } ?: throw IllegalStateException("无法写入下载目录")
                values.clear()
                values.put(MediaStore.Downloads.IS_PENDING, 0)
                resolver.update(uri, values, null, null)
            } catch (t: Throwable) {
                runCatching { resolver.delete(uri, null, null) }
                throw t
            }
        } else {
            val dir = File(
                Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DOWNLOADS),
                "FST",
            ).apply { mkdirs() }
            var target = File(dir, name)
            var i = 1
            while (target.exists()) {
                val dot = name.lastIndexOf('.')
                val base = if (dot > 0) name.substring(0, dot) else name
                val ext = if (dot > 0) name.substring(dot) else ""
                target = File(dir, "$base-$i$ext")
                i++
            }
            src.copyTo(target, overwrite = false)
        }
    }

    /** 按扩展名猜 MIME；猜不出时用二进制流，避免相册/播放器误判。 */
    private fun mimeOf(name: String): String = when (name.substringAfterLast('.', "").lowercase()) {
        "jpg", "jpeg" -> "image/jpeg"
        "png" -> "image/png"
        "gif" -> "image/gif"
        "webp" -> "image/webp"
        "mp4" -> "video/mp4"
        "mp3" -> "audio/mpeg"
        "pdf" -> "application/pdf"
        "txt", "md" -> "text/plain"
        "zip" -> "application/zip"
        "apk" -> "application/vnd.android.package-archive"
        else -> "application/octet-stream"
    }
}
