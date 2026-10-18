import fs from 'node:fs'
import { createRequire } from 'node:module'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { defineConfig, loadEnv } from 'vite'
import vue from '@vitejs/plugin-vue'
import Components from 'unplugin-vue-components/vite'
import AutoImport from 'unplugin-auto-import/vite'
import ElementPlus from 'unplugin-element-plus/vite'
import { ElementPlusResolver } from 'unplugin-vue-components/resolvers'
import tailwindcss from '@tailwindcss/vite'

const __filename = fileURLToPath(import.meta.url)
const __dirname = path.dirname(__filename)

// 样式 deep import 放进 include 会预编译 100+ 份 SCSS，冷启动要两分钟。
// 放进 exclude：按页现编、不进 optimize 集合，换页不会 reloading。
function elementPlusStyleExcludes(): string[] {
  const require = createRequire(import.meta.url)
  const epRoot = path.dirname(require.resolve('element-plus/package.json'))
  const componentsDir = path.join(epRoot, 'es/components')
  if (!fs.existsSync(componentsDir)) return []
  const ids: string[] = []
  for (const dirent of fs.readdirSync(componentsDir, { withFileTypes: true })) {
    if (!dirent.isDirectory()) continue
    const styleFile = path.join(componentsDir, dirent.name, 'style', 'index.mjs')
    if (fs.existsSync(styleFile)) {
      ids.push(`element-plus/es/components/${dirent.name}/style/index`)
    }
  }
  return ids
}

export default ({ mode }: { mode: string }) => {
  const root = process.cwd()
  const env = loadEnv(mode, root)
  const { VITE_VERSION, VITE_BASE_URL } = env

  console.log(`🚀 VERSION = ${VITE_VERSION}`)

  return defineConfig({
    define: {
      __APP_VERSION__: JSON.stringify(VITE_VERSION)
    },
    base: VITE_BASE_URL,
    server: {
      host: '0.0.0.0',
      port: 5730,
      hmr: true,
      warmup: { clientFiles: ['./src/main.ts', './src/App.vue'] }
    },
    resolve: {
      // 避免多份 Vue 实例导致 vue-i18n 与 runtime 初始化不一致
      dedupe: ['vue'],
      alias: {
        '@': fileURLToPath(new URL('./src', import.meta.url)),
        '@views': resolvePath('src/views'),
        '@imgs': resolvePath('src/assets/images'),
        '@icons': resolvePath('src/assets/icons'),
        '@utils': resolvePath('src/utils'),
        '@stores': resolvePath('src/store'),
        '@styles': resolvePath('src/assets/styles')
      }
    },
    build: {
      target: 'es2015',
      dynamicImportVarsOptions: {
        warnOnError: true,
        exclude: [],
        include: ['src/views/**/*.vue']
      },
      rollupOptions: {
        output: {
          // Vite 8 使用 Rolldown：manualChunks 已废弃，改用 codeSplitting.groups
          // vue-i18n 必须与 vue 同 chunk，否则 Rolldown 会丢失 init_runtime_dom_esm_bundler
          codeSplitting: {
            groups: [
              {
                name: 'vue-vendor',
                test: /node_modules[\\/](vue|vue-router|pinia|vue-i18n|@intlify|@vueuse[\\/])/
              },
              { name: 'element-plus', test: /node_modules[\\/]element-plus/ }
            ]
          },
          chunkFileNames: 'assets/[name]-[hash].js',
          entryFileNames: 'assets/[name]-[hash].js',
          assetFileNames: 'assets/[name]-[hash].[ext]'
        }
      },
      outDir: 'dist',
      chunkSizeWarningLimit: 2000,
      minify: 'esbuild'
    },
    plugins: [
      vue(),
      tailwindcss(),
      AutoImport({
        imports: ['vue', 'vue-router', 'pinia', '@vueuse/core'],
        dts: 'src/types/import/auto-imports.d.ts',
        resolvers: [ElementPlusResolver({ importStyle: false })],
        eslintrc: {
          enabled: true,
          filepath: './.auto-import.json',
          globalsPropValue: true
        }
      }),
      Components({
        dts: 'src/types/import/components.d.ts',
        resolvers: [ElementPlusResolver({ importStyle: 'sass' })]
      }),
      ElementPlus({ useSource: true })
    ],
    optimizeDeps: {
      // Rolldown 1.0.2+ 预构建 vue-i18n 时会丢失 init_runtime_dom_esm_bundler 引用
      exclude: ['vue-i18n', ...elementPlusStyleExcludes()],
      holdUntilCrawlEnd: false,
      include: ['vue', 'element-plus/es']
    },
    css: {
      preprocessorOptions: {
        scss: {
          // 避免 additionalData 再注入到 el-light/mixin 自身造成循环 @use，导致 $button 主题未生效
          additionalData: (content: string, filepath: string) => {
            if (
              filepath.replace(/\\/g, '/').includes('/styles/core/el-light.scss') ||
              filepath.replace(/\\/g, '/').includes('/styles/core/mixin.scss')
            ) {
              return content
            }
            return `
            @use "@styles/core/el-light.scss" as *;
            @use "@styles/core/mixin.scss" as *;
            ${content}`
          }
        }
      },
      postcss: {
        plugins: [
          {
            postcssPlugin: 'internal:charset-removal',
            AtRule: {
              charset: (atRule) => {
                if (atRule.name === 'charset') atRule.remove()
              }
            }
          }
        ]
      }
    }
  })
}

function resolvePath(paths: string) {
  return path.resolve(__dirname, paths)
}