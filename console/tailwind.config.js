/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{js,jsx}'],
  // 本项目 1:1 还原原型,视觉完全由 src/index.css 的设计令牌(CSS 变量 + 语义类)
  // 决定。关闭 preflight,避免 Tailwind 的全局 reset 改写既有排版,破坏 1:1。
  // 工具类仍可按需使用。
  corePlugins: { preflight: false },
  theme: { extend: {} },
  plugins: [],
};
