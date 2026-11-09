package com.fst.phone.service

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.IBinder
import androidx.core.app.NotificationCompat
import com.fst.phone.MainActivity
import com.fst.phone.R
import com.fst.phone.data.FstRepository

/**
 * FstService 是 FST 在手机上的「常驻外壳」。
 *
 * 为什么需要前台服务：
 *  1. 移动操作系统会冻结/回收后台进程，节点一旦被杀，libp2p 连接与 mDNS 注册
 *     全部消失，别人就再也发现不到这台手机。
 *  2. 文件传输可能持续数分钟，必须有前台服务类型（dataSync）才不会在息屏后被
 *     限流或杀死。
 *
 * 生命周期：MainActivity 每次启动都会 startForegroundService（幂等）；用户在
 * 最近任务里划掉应用后由系统决定是否重启，返回 START_STICKY 表示愿意被重启。
 */
class FstService : Service() {

    companion object {
        private const val CHANNEL_ID = "fst-connection"
        private const val NOTIFICATION_ID = 1001

        /**
         * 启动服务；已启动时只是把服务提到前台。
         *
         * 参数:
         *  - context: Context — 调用方上下文
         */
        fun start(context: Context) {
            val intent = Intent(context, FstService::class.java)
            if (android.os.Build.VERSION.SDK_INT >= android.os.Build.VERSION_CODES.O) {
                context.startForegroundService(intent)
            } else {
                context.startService(intent)
            }
        }
    }

    override fun onCreate() {
        super.onCreate()
        createChannel()
        startForeground(NOTIFICATION_ID, buildNotification())
        FstRepository.start(this)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        FstRepository.start(this)
        return START_STICKY
    }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onDestroy() {
        FstRepository.stop()
        super.onDestroy()
    }

    /** 创建通知渠道；Android 8+ 必须先建渠道才能发通知。 */
    private fun createChannel() {
        val manager = getSystemService(NotificationManager::class.java)
        val channel = NotificationChannel(
            CHANNEL_ID,
            getString(R.string.notif_channel_name),
            NotificationManager.IMPORTANCE_LOW,
        ).apply {
            description = getString(R.string.notif_channel_desc)
            setShowBadge(false)
        }
        manager.createNotificationChannel(channel)
    }

    /** 构建常驻通知；点击回到主界面。 */
    private fun buildNotification(): Notification {
        val pending = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java).apply {
                flags = Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP
            },
            PendingIntent.FLAG_IMMUTABLE,
        )
        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle(getString(R.string.app_name))
            .setContentText(getString(R.string.notif_running))
            .setSmallIcon(android.R.drawable.stat_sys_upload_done)
            .setContentIntent(pending)
            .setOngoing(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .build()
    }
}
