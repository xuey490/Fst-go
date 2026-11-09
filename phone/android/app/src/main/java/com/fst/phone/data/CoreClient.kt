package com.fst.phone.data

import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive

/**
 * CoreClient 是本机 Go 守护进程的 HTTP 客户端。
 *
 * 约定（与 phone/daemon 一致）：
 *  - 只连 127.0.0.1 上的随机端口，端口由守护进程启动时打印。
 *  - 每个请求带 X-FST-Token；令牌由 App 每次启动随机生成，只通过进程环境变量传递。
 *  - 返回体是 JSON：GET 直接是数组/对象，POST 是 {"data":{...},"error":"..."}。
 *
 * 线程约定：所有方法都是阻塞调用，必须在 IO 线程使用。
 */
class CoreClient(private val port: Int, private val token: String) {

    /** 基地址，固定回环，禁止改成对外监听。 */
    private val base: String = "http://127.0.0.1:$port"

    /**
     * 发起 GET 请求。
     *
     * 参数:
     *  - path: String — 以 / 开头的路径（含查询串）
     *
     * 返回:
     *  - String — 响应体 JSON
     *
     * 异常:
     *  - IOException — 连接失败或 HTTP 非 2xx
     */
    fun get(path: String): String = request("GET", path, null)

    /**
     * 发起 POST 请求。
     *
     * 参数:
     *  - path: String — 以 / 开头的路径
     *  - body: String — JSON 请求体
     *
     * 返回:
     *  - String — 响应体 JSON
     *
     * 异常:
     *  - IOException — 连接失败或 HTTP 非 2xx
     */
    fun post(path: String, body: String): String = request("POST", path, body)

    /** 实际执行请求；SSE 长连接不走这里。 */
    private fun request(method: String, path: String, body: String?): String {
        val conn = URL(base + path).openConnection() as HttpURLConnection
        try {
            conn.requestMethod = method
            conn.connectTimeout = 5_000
            conn.readTimeout = 20_000
            conn.setRequestProperty(TOKEN_HEADER, token)
            if (body != null) {
                conn.doOutput = true
                conn.setRequestProperty("Content-Type", "application/json; charset=utf-8")
                conn.outputStream.use { it.write(body.toByteArray(Charsets.UTF_8)) }
            }
            val code = conn.responseCode
            val text = (if (code in 200..299) conn.inputStream else conn.errorStream)
                ?.bufferedReader(Charsets.UTF_8)?.use { it.readText() } ?: ""
            if (code !in 200..299) throw IOException("HTTP $code: $text")
            return text
        } finally {
            conn.disconnect()
        }
    }

    /**
     * 订阅事件流（SSE），阻塞直到连接断开。
     *
     * 参数:
     *  - onEvent: (name: String, payload: String) -> Unit — 每收到一个事件回调一次；在读取线程上执行
     *
     * 异常:
     *  - IOException — 连接失败（调用方可重连）
     */
    fun streamEvents(onEvent: (String, String) -> Unit) {
        val conn = URL("$base/events").openConnection() as HttpURLConnection
        try {
            conn.requestMethod = "GET"
            conn.connectTimeout = 5_000
            conn.readTimeout = 0 // 长连接：不设读超时，由对端心跳保活
            conn.setRequestProperty(TOKEN_HEADER, token)
            conn.inputStream.bufferedReader(Charsets.UTF_8).use { reader ->
                var event = "message"
                while (true) {
                    val line = reader.readLine() ?: break
                    when {
                        line.startsWith("event: ") -> event = line.removePrefix("event: ").trim()
                        line.startsWith("data: ") -> {
                            onEvent(event, line.removePrefix("data: ").trim())
                            event = "message"
                        }
                    }
                }
            }
        } finally {
            conn.disconnect()
        }
    }

    /**
     * 从 POST 响应里取 error 字段；空表示成功。
     *
     * 参数:
     *  - body: String — 守护进程返回的 JSON
     *
     * 返回:
     *  - String? — 错误文案；成功为 null
     */
    fun errorOf(body: String): String? = runCatching {
        FstJson.parseToJsonElement(body).jsonObject["error"]?.jsonPrimitive?.content
    }.getOrNull()?.takeIf { it.isNotBlank() }

    companion object {
        /** 鉴权头名称，必须与 phone/daemon 的 tokenHeader 一致。 */
        const val TOKEN_HEADER = "X-FST-Token"
    }
}
