import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 构建产物输出到 dist/，由 Go 通过 //go:embed all:web/dist 嵌入并提供服务。
// base: './' 让资源以相对路径引用，既能由 Go 在根路径 "/" 提供，也能直接 file:// 预览。
export default defineConfig({
  base: './',
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    // 开发模式下把 /api 代理到 Go 后端，避免跨域。
    // 用 127.0.0.1 而非 localhost：后端默认绑 127.0.0.1 (仅 IPv4)，
    // localhost 在 macOS 会先解析到 ::1 (IPv6)，偶发 ECONNREFUSED。
    proxy: {
      '/api': 'http://127.0.0.1:8080',
    },
  },
})
