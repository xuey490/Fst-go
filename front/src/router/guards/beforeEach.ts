/**
 * 路由守卫（FST 版：无登录、无权限，仅在首次导航时注册本地菜单路由）
 */
import type { Router, RouteLocationNormalized, RouteLocationRaw } from 'vue-router'
import { nextTick } from 'vue'
import NProgress from 'nprogress'
import { useSettingStore } from '@/store/modules/setting'
import { useMenuStore } from '@/store/modules/menu'
import { useWorktabStore } from '@/store/modules/worktab'
import { useFstStore } from '@/store/modules/fst'
import { setWorktab } from '@/utils/navigation'
import { setPageTitle } from '@/utils/router'
import { loadingService } from '@/utils/ui'
import { useCommon } from '@/hooks/core/useCommon'
import { RouteRegistry, MenuProcessor } from '../core'

let routeRegistry: RouteRegistry | null = null
const menuProcessor = new MenuProcessor()
let pendingLoading = false
// 初始化失败后不再重试，避免死循环
let routeInitFailed = false
let routeInitInProgress = false

export const getPendingLoading = () => pendingLoading
export const resetPendingLoading = () => {
  pendingLoading = false
}

export function setupBeforeEachGuard(router: Router): void {
  routeRegistry = new RouteRegistry(router)
  router.beforeEach((to) => handleRouteGuard(to, router))
}

function closeLoading(): void {
  if (!pendingLoading) return
  nextTick(() => {
    loadingService.hideLoading()
    pendingLoading = false
  })
}

async function handleRouteGuard(
  to: RouteLocationNormalized,
  router: Router
): Promise<RouteLocationRaw | boolean | undefined> {
  if (useSettingStore().showNprogress) NProgress.start()

  if (routeInitFailed) {
    return to.matched.length > 0 ? true : { name: 'Exception500', replace: true }
  }

  if (!routeRegistry?.isRegistered()) {
    if (routeInitInProgress) return false
    return handleDynamicRoutes(to, router)
  }

  if (to.path === '/') {
    const { homePath } = useCommon()
    if (homePath.value && homePath.value !== '/') return { path: homePath.value, replace: true }
  }

  if (to.matched.length > 0) {
    setWorktab(to)
    setPageTitle(to)
    return true
  }
  return { name: 'Exception404' }
}

async function handleDynamicRoutes(
  to: RouteLocationNormalized,
  router: Router
): Promise<RouteLocationRaw | boolean> {
  routeInitInProgress = true
  pendingLoading = true
  loadingService.showLoading()

  try {
    // 本机身份（昵称/PeerID）；后端未就绪（纯浏览器调试）时不阻塞界面
    try {
      await useFstStore().init()
    } catch (e) {
      console.warn('[FST] init failed', e)
    }

    const menuList = await menuProcessor.getMenuList()
    routeRegistry?.register(menuList)
    const menuStore = useMenuStore()
    menuStore.setMenuList(menuList)
    menuStore.addRemoveRouteFns(routeRegistry?.getRemoveRouteFns() || [])
    useWorktabStore().validateWorktabs(router)

    routeInitInProgress = false
    const { homePath } = useCommon()
    // 目标路径存在则原地重新导航，否则回首页
    const target = to.path === '/' ? homePath.value || '/device' : to.path
    return { path: target, query: to.query, hash: to.hash, replace: true }
  } catch (error) {
    console.error('[RouteGuard] 路由注册失败:', error)
    closeLoading()
    routeInitFailed = true
    routeInitInProgress = false
    return { name: 'Exception500', replace: true }
  }
}