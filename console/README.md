# Mooncell Console

内网自动化部署平台控制台 —— 由单文件离线原型 `Mooncell Console（离线版）.html` 重构为标准全栈工程。

技术栈:**Vite + React 18 + Tailwind** 前端,**Go** 后端,**SQLite** 存储。生产态打包为**单个二进制**(前端经 `go:embed` 嵌入),部署只需:二进制 + `config.toml` + 自动生成的 sqlite 文件。

## 运行

### 开发态

前端 5173 热更新,Go 后端 8787,`/api` 走 Vite 代理:

```bash
pnpm install
pnpm dev          # 同时起 vite 与 go run .
```

### 生产态(单二进制)

```bash
pnpm build                 # 1. 构建前端到 dist/
go build -o mooncell .     # 2. 把 dist/ 嵌入,产出单二进制
./mooncell                 # http://localhost:8787

# 或一条命令完成构建 + 打包
pnpm dist
```

部署时只需拷贝 `mooncell` 二进制和 `config.toml` 到目标机即可运行(sqlite 文件首次启动自动创建)。`go build` 前必须先有 `dist/`(由 `pnpm build` 生成),否则 `go:embed` 找不到资源会编译失败。

默认管理员账号:**admin / jch@9388**(仅在用户表为空时由 `config.toml` 的 `[admin]` 种入,bcrypt 存储;库中已有用户后修改配置不影响既有账号)。

### 配置 `config.toml`

```toml
[server]
addr = "0.0.0.0"        # 监听地址,127.0.0.1 仅本机
port = 8787

[database]
path = "mooncell.db"    # sqlite 文件路径,首次运行自动创建

[session]
ttl_hours = 168         # 会话有效期(小时),168 = 7 天

[admin]
username = "admin"
password = "jch@9388"

[agent]                  # Console 主动连接的默认 Agent(单机版指向本机)
addr  = "127.0.0.1:9100"
token = "mc_ag_change_me"   # 须与 Agent 端 token 一致

[cabinet]
dir         = "cabinet"  # 文件柜二进制落盘目录
anon_upload = false      # 是否允许免登录匿名上传(POST /api/pub/cabinet)
```

配置文件缺失或字段缺省时使用内置默认值(同上),可只覆盖部分字段。
多 Agent 在运行时由「Agent 管理」页注册(存 SQLite),应用按 `agentId` 路由。

## 设计说明

- **1:1 还原**:原型的视觉完全由一套自定义 CSS 设计令牌(CSS 变量 + 语义类 `.btn`/`.card`/`.console` …)决定,**不是 Tailwind**。迁移时这套样式 `src/index.css` 原样保留;Tailwind 按技术栈要求接入,但关闭 `preflight`(避免全局 reset 改写既有排版),工具类可按需使用。字体(IBM Plex Mono + Instrument Sans 共 19 个 woff2)本地打包,保证离线一致。
- **真实后端能力(已超原型)**:登录/RBAC(admin/operator/viewer,服务端强制鉴权)、Agent 代理(部署/还原/日志/备份,据已存类型化配置服务端生成请求 + releaseId 幂等)、多 Agent 注册与路由、文件柜(二进制落盘 + 提取码 + 匿名上传 + 过期清理)、审计与发布记录服务端权威落库,均为真实后端。`src/lib/data.js` 的 seed 仅作首启演示数据;进程类/static/tomcat 应用经 UI 走真机部署。完成度见 [../README.md](../README.md)。
- **登录为真实后端**:`POST /api/login` 校验 SQLite 中的 bcrypt 口令,签发随机 token 写入 `sessions` 表,并以 **httpOnly cookie**(`mc_sid`)维持会话;`GET /api/session` 查询登录态,`POST /api/logout` 注销。前端挂载时查询会话决定进入登录页或控制台。
- **单二进制部署**:后端用 Go,前端构建产物经 `//go:embed all:dist` 编译进二进制,运行时从内存映像直接服务静态资源(无磁盘 IO)。sqlite 用纯 Go 驱动 `modernc.org/sqlite`(无 CGO),保证交叉编译与单文件部署的纯粹性。
- **Chrome 92+ 兼容**:Vite `build.target` / `esbuild.target` 设为 `chrome92`,现代语法降级到该目标可运行的形态。
- 原型自带的 `TweaksPanel`(托管编辑器的悬浮调试面板,向 `window.parent` postMessage)属于编辑器 chrome、正常运行永不显示,迁移时丢弃;App 真正用到的暗色模式 + 日志字号保留为 `src/lib/tweaks.js`,持久化到 localStorage(暗色模式仍可由顶栏月亮按钮切换)。

## 结构

```
index.html            入口
vite.config.js        chrome92 目标 + /api 代理
tailwind.config.js    preflight 关闭
config.toml           后端运行配置(监听/db/会话/管理员)
go.mod                Go 模块定义
main.go               入口:加载配置 + 建库 + 路由 + 嵌入式静态托管(go:embed dist)
config.go             config.toml 解析(BurntSushi/toml)+ 内置默认值
db.go                 modernc sqlite:users / sessions + 种子管理员 + 会话读写
auth.go               登录 / 注销 / 会话 API + httpOnly cookie
src/
  main.jsx            挂载
  App.jsx             根组件:Store + 路由 + 主题 + 会话
  index.css           全局样式(原型 1:1) + @font-face
  fonts/              19 个 woff2
  lib/
    data.js           领域 mock 数据 + 工具
    api.js            登录 API 客户端
    tweaks.js         主题/日志字号
  components/
    primitives.jsx    UI 原语(Btn/Badge/Dialog/Console …)
    Shell.jsx         侧边栏 + 顶栏
    pipeline.jsx      部署流水线模拟 + 部署/还原对话框
  pages/
    Login.jsx         登录页 + 初始化向导
    Overview.jsx      总览 / 文件柜 / 审计日志
    Apps.jsx          应用列表 + 新建向导
    AppDetail.jsx     应用详情(概览/记录/备份/日志/配置)
```
