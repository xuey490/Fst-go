import App from './App.vue'
import { createApp } from 'vue'
import { initStore } from './store'                 // Store
import { initRouter } from './router'               // Router
import language from './locales'                    // 国际化
import '@styles/core/tailwind.css'                  // tailwind
import '@styles/index.scss'                         // 样式
import '@utils/sys/console'                         // 控制台输出内容
import { setupGlobDirectives } from './directives'
import { setupErrorHandle } from './utils/sys/error-handle'
import { setupOfflineIcons } from './utils/ui/iconify-loader' // 离线图标（内网无网络也能显示）

document.addEventListener(
  'touchstart',
  function () {},
  { passive: false }
)

// 图标集必须在任何组件渲染前注册：否则 <Icon> 会去 api.iconify.design 拉取
setupOfflineIcons()

const app = createApp(App)
initStore(app)
initRouter(app)
setupGlobDirectives(app)
setupErrorHandle(app)

app.use(language)
app.mount('#app')