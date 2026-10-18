/**
 * FST 前端通用小工具：头像 data URL 拼接、字节数格式化。
 * 只做纯展示转换，不涉及网络请求与业务状态。
 */

/**
 * 把 Go 侧返回的 base64 头像转成可直接用于 <img>/ElAvatar 的 data URL。
 * Go 只返回裸 base64（无 data: 前缀），这里按 magic 前缀猜 MIME，避免 JPEG 被标成 PNG。
 *
 * 参数:
 *   - b64: string | undefined — base64 字符串；空串/undefined 表示没有头像
 *
 * 返回:
 *   - string — data URL；没有头像时返回空串（组件回退到插槽内容）
 */
export const avatarURL = (b64?: string) => {
  if (!b64) return ''
  const mime = b64.startsWith('/9j/')
    ? 'jpeg'
    : b64.startsWith('R0lGOD')
      ? 'gif'
      : b64.startsWith('UklGR')
        ? 'webp'
        : 'png'
  return `data:image/${mime};base64,${b64}`
}

/**
 * 把字节数格式化为 B / KB / MB / GB 可读文本（保留一位小数，小于 10 才留小数）。
 *
 * 参数:
 *   - n: number | undefined — 字节数；undefined/0 返回空串，便于模板直接展示
 *
 * 返回:
 *   - string — 例如 "512 B" / "1.5 KB" / "8.0 MB"；无有效数值时为空串
 */
export const formatBytes = (n?: number) => {
  if (!n || n <= 0) return ''
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${i === 0 ? v : v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`
}
