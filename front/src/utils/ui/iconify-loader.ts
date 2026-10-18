/**
 * 离线图标加载器
 *
 * Iconify 的 <Icon> 在图标未注册时会向 api.iconify.design 发起网络请求；
 * 完全离线的局域网里这就表现为图标全部空白。启动时把图标集数据直接写进
 * Iconify 的本地存储（addCollection），此后所有图标都从内存解析，零外网请求。
 *
 * 图标集与源码实际使用情况（2026-10-17 统计）：
 *   - ri: 58 个静态引用（菜单/按钮/状态图标），全量注册 1043KB
 *   - line-md / svg-spinners: 当前无静态引用，但模板动效与后续页面可能用到，体积 1446KB / 55KB
 * ponytail: 若需要极致包体，可只注册 ri，或改为构建期扫描源码生成 58 个图标的子集；
 * 当前优先保证离线可用与「随便加图标都能显示」，体积代价约 600KB（gzip 后）可接受。
 *
 * @module utils/ui/iconify-loader
 */
import { addCollection } from '@iconify/vue'
import type { IconifyJSON } from '@iconify/vue'
import riIcons from '@iconify-json/ri/icons.json'
import svgSpinners from '@iconify-json/svg-spinners/icons.json'
import lineMd from '@iconify-json/line-md/icons.json'

/**
 * 把三套图标集注册到 Iconify 本地存储，供 <ArtSvgIcon> / <Icon> 离线解析。
 * 必须在 app.mount 之前调用一次；重复调用是幂等的（后注册覆盖同名图标集）。
 *
 * 参数:
 *   - 无
 *
 * 返回:
 *   - void
 */
export function setupOfflineIcons(): void {
  addCollection(riIcons as IconifyJSON)
  addCollection(svgSpinners as IconifyJSON)
  addCollection(lineMd as IconifyJSON)
}
