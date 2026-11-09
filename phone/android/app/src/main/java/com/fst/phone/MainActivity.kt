package com.fst.phone

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.core.content.ContextCompat
import com.fst.phone.service.FstService
import com.fst.phone.ui.FstApp
import com.fst.phone.ui.theme.FstTheme

/**
 * 主界面。职责很小：申请权限、拉起前台服务、挂载 Compose。
 *
 * 权限策略：
 *  - Android 13+ 需要 POST_NOTIFICATIONS（前台服务通知）与 NEARBY_WIFI_DEVICES
 *    （本地网络访问；从 Android 17 起属于 NEARBY_DEVICES 组）。
 *  - Android 8/9 需要 WRITE_EXTERNAL_STORAGE 才能把文件导出到公共下载目录。
 *  - 其余权限是普通权限，安装即授予。
 *
 * 权限被拒绝不阻塞启动：节点仍能运行，只是可能收不到组播包或无法导出到公共目录。
 */
class MainActivity : ComponentActivity() {

    private val permissionLauncher = registerForActivityResult(
        ActivityResultContracts.RequestMultiplePermissions(),
    ) { /* 拒绝也继续运行，界面里会用提示告知能力受限 */ }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        requestRuntimePermissions()
        FstService.start(this)
        setContent {
            FstTheme {
                FstApp()
            }
        }
    }

    /** 收集当前系统版本下尚未授予的运行时权限并发起请求。 */
    private fun requestRuntimePermissions() {
        val wanted = buildList {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
                add(Manifest.permission.POST_NOTIFICATIONS)
                add(Manifest.permission.NEARBY_WIFI_DEVICES)
            }
            if (Build.VERSION.SDK_INT <= Build.VERSION_CODES.P) {
                add(Manifest.permission.WRITE_EXTERNAL_STORAGE)
            }
        }
        val missing = wanted.filter {
            ContextCompat.checkSelfPermission(this, it) != PackageManager.PERMISSION_GRANTED
        }
        if (missing.isNotEmpty()) {
            permissionLauncher.launch(missing.toTypedArray())
        }
    }
}
