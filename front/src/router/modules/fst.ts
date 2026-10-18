import { AppRouteRecord } from '@/types/router'

// 一级菜单（无 children）：RouteTransformer 会自动用 Layout 包裹
const page = (path: string, name: string, title: string, icon: string): AppRouteRecord => ({
  path,
  name,
  component: '/fst' + path,
  meta: { title, icon, keepAlive: true }
})

export const fstRoutes: AppRouteRecord[] = [
  page('/device', 'Device', 'menus.fst.device', 'ri:radar-line'),
  page('/chat', 'Chat', 'menus.fst.chat', 'ri:chat-3-line'),
  page('/transfer', 'Transfer', 'menus.fst.transfer', 'ri:swap-box-line'),
  page('/settings', 'Settings', 'menus.fst.settings', 'ri:settings-3-line')
]