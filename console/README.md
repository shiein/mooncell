# Mooncell Console

内网自动化部署平台控制台。前端 Vite + React 18 + Tailwind 经 `go:embed` 嵌入,Go 后端 + SQLite(`modernc.org/sqlite`,无 CGO),生产态打包为单二进制。

## 编译

```bash
# 开发态:前端 5173 热更新,Go 后端 8787,/api 走 Vite 代理
pnpm install
pnpm dev

# 生产态(单二进制):先构建前端再嵌入打包
pnpm build                 # 1. 构建前端到 dist/(go build 前必须先有 dist/,否则 go:embed 找不到资源会编译失败)
go build -o mooncell .     # 2. 把 dist/ 嵌入,产出单二进制
./mooncell                 # http://localhost:8787

pnpm dist                  # 或一条命令完成 build + 打包
```

部署只需拷贝 `mooncell` 二进制 + `config.toml` 到目标机(sqlite 文件首次启动自动创建)。

## 配置 `config.toml`

> ⚠️ 安全:默认监听 `127.0.0.1`(仅本机)。**对外监听(`0.0.0.0`/`::`)前必须先改掉默认管理员密码与 Agent token**——否则 Console 拒绝启动(`拒绝以不安全配置对外启动`)。

```toml
[server]
addr = "127.0.0.1"      # 监听地址,默认仅本机;对外请改 0.0.0.0 并务必先改下方默认密码/token
port = 8787

[database]
path = "mooncell.db"    # sqlite 文件路径,首次运行自动创建

[session]
ttl_hours = 168         # 会话有效期(小时),168 = 7 天

[admin]
username = "admin"
password = "1qaz@WSX"   # 默认口令;对外监听必须改,否则启动被拒(仅用户表为空时种入,bcrypt 存储)

[agent]                 # Console 主动连接的默认 Agent(单机版指向本机)
addr  = "127.0.0.1:9100"
token = "mc_ag_change_me"   # 须与 Agent 端 token 一致;对外监听必须改,否则启动被拒

[cabinet]
dir           = "cabinet"   # 文件柜二进制落盘目录
anon_upload   = false       # 是否允许免登录匿名上传(/drop 页 + POST /api/pub/cabinet)
max_upload_mb = 300         # 文件柜单文件上限(MB)

[agent_bin]
dir = "agentbin"        # Agent 升级包(按架构上传)的存储目录

[deploy]
max_upload_mb = 1024    # 部署制品上传传输层硬上限(MB),超限 413
```

配置文件缺失或字段缺省时使用内置默认值(同上),可只覆盖部分字段。多 Agent 在运行时由「Agent 管理」页注册(存 SQLite),应用按 `agentId` 路由。
