import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// 目标:兼容 Chrome 92+。esbuild 会把现代语法降级到该目标可运行的形态。
const LEGACY_TARGET = ['chrome92', 'edge92', 'firefox91', 'safari15'];

export default defineConfig({
  plugins: [react()],
  define: {
    // Chrome 92 无 structuredClone（98+），esbuild 只降语法不降 API。
    // 把依赖（@xyflow/react）里的调用重命名到 __mcStructuredClone，
    // 由 index.html 注入的同型 polyfill 提供实现。
    structuredClone: '__mcStructuredClone',
  },
  build: {
    target: LEGACY_TARGET,
    cssTarget: LEGACY_TARGET,
    outDir: 'dist',
  },
  esbuild: {
    target: 'chrome92',
  },
  optimizeDeps: {
    esbuildOptions: { target: 'chrome92' },
  },
  server: {
    port: 5173,
    // 开发态:前端 5173,后端 Hono 8787,/api 走代理
    proxy: {
      '/api': 'http://localhost:8787',
    },
  },
});
