package com.fst.phone

import android.app.Application

/**
 * 应用入口。当前不持有全局状态：核心实例由 [com.fst.phone.data.FstRepository]
 * 单例管理，生命周期绑定到前台服务，避免 Activity 重建导致节点重启。
 */
class FstApp : Application()
