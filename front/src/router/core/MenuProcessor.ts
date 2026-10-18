/**
 * 菜单处理器：FST 菜单固定在前端（router/modules/fst.ts），仅做路径规范化
 */
import type { AppRouteRecord } from '@/types/router'
import { asyncRoutes } from '../routes/asyncRoutes'

export class MenuProcessor {
  async getMenuList(): Promise<AppRouteRecord[]> {
    return this.normalizeMenuPaths([...asyncRoutes])
  }

  validateMenuList(menuList: AppRouteRecord[]): boolean {
    return Array.isArray(menuList) && menuList.length > 0
  }

  /** 相对路径 -> 完整路径 */
  private normalizeMenuPaths(menuList: AppRouteRecord[], parentPath = ''): AppRouteRecord[] {
    return menuList.map((item) => {
      const fullPath = this.buildFullPath(item.path || '', parentPath)
      const children = item.children?.length
        ? this.normalizeMenuPaths(item.children, fullPath)
        : item.children
      return { ...item, path: fullPath, children }
    })
  }

  private buildFullPath(path: string, parentPath: string): string {
    if (!path) return ''
    if (path.startsWith('/')) return path
    return parentPath ? parentPath.replace(/\/$/, '') + '/' + path : '/' + path
  }
}