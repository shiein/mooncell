# Mooncell

> 面向**内网 / 单机 / 离线**交付环境的轻量自动化部署平台 —— 上传即部署、自动备份、一键还原、在线看日志。

Mooncell 用两个**零依赖的 Go 单二进制**(Console 控制台 + Agent 部署代理)取代手工 `scp + systemctl` 的部署方式。前端经 `go:embed` 编进 Console 二进制,数据存纯 Go 的 SQLite,**无需 Docker、无需运行时依赖**,特别适合 UOS / 麒麟等国产化、离线、内网场景。

---

## ✨ 特性

- **多种部署类型**:go-binary、java-jar、python、node(进程类,systemd / pm2 托管)+ static-nginx(软链原子切换)+ tomcat-war(容器换 WAR)。
- **上传即部署**:浏览器上传制品 → 自动流水线(备份 → 停 → 替换 → 启动 → 健康检查),**失败自动回滚**到上一个可用版本。
- **一键还原**:列历史备份,用任意历史制品重跑部署流水线;还原前自动备份当前版本。
- **实时日志**:journald / pm2 logs / 声明文件 `tail -F` 经 SSE 推到前端,支持暂停、关键字高亮、时间范围 gzip 下载。
- **多 Agent**:一台 Console 管多台目标机,应用按 `agentId` 路由;Agent 启动自检能力清单(java/pm2/nginx/tomcat…),前端按真实能力过滤可选 Runner。
- **分块上传 + 断点续传**:大制品(>16MB)自动分块,网络中断可凭进度续传。
- **安全边界**:RBAC(admin/operator/viewer)、bcrypt + httpOnly 会话、Agent 共享 Token(常数时间比较)、路径白名单(`EvalSymlinks` 防软链逃逸)、压缩包安全解包(防 zip-slip / 炸弹)、Agent 只暴露**类型化 API**(无任意 shell)。
- **审计与幂等**:所有写操作服务端权威落审计;`op/appId/releaseId` + 请求指纹幂等,重试不重复部署、换制品复用 releaseId 会被拒。
- **文件柜**:内网临时文件中转(二进制落盘 + 提取码 + 公开直链 + 过期自动清理)。
- **离线交付**:`install.sh` 一键把两端装为 systemd 服务、自动生成共享 Token 打通;支持 `upgrade` / `uninstall`。

---

## 🏗 架构

```
┌──────────────────────────────────────────┐
│        Console(Go 单二进制)              │
│  React + Tailwind(go:embed)             │
│  ←→ Go net/http  ·  SQLite(纯 Go,无 CGO) │
└───────────────┬──────────────────────────┘
                │ HTTP + 共享 Token(SSE 流式日志/部署)
                ▼
┌──────────────────────────────────────────┐
│         Agent(Go 单二进制,目标机)        │
│  Deployer 插件 · Runner 抽象 · 备份 · 日志流 │
└───────────────┬──────────────────────────┘
                ▼
      systemd / pm2 / Tomcat 容器 / nginx 目录
```

- **Console**:Web 控制台,管理应用、触发部署、看日志、备份还原、用户/Agent 管理、文件柜。
- **Agent**:目标机常驻服务,执行真实的落盘、进程起停、备份、日志读取。
- 单机可同机部署;架构上一台 Console 天然支持管多台 Agent。
- 设计细节见 [docs/deploy-platform-design-v1.md](docs/deploy-platform-design-v1.md)。

---

## 🚀 快速开始

### 方式一:离线安装(推荐,生产)

在**联网构建机**打包,把产出目录拷到**目标机**离线安装。

```bash
# 1) 构建机(需 go + pnpm):产出离线 bundle
ARCH=amd64 deploy/build-release.sh           # 国产化 arm64:ARCH=arm64
# → deploy/release/mooncell-amd64/{mooncell-console, mooncell-agent, install.sh}

# 2) 目标机(root):拷入整个 bundle 目录后
./install.sh install        # 装为 systemd 服务,自动生成共享 Token 打通两端、打印初始管理员口令
./install.sh status         # 查看服务状态与访问地址
./install.sh upgrade        # 仅换二进制并重启,保留配置与数据库
./install.sh uninstall      # 卸载(默认留数据;--purge 连数据一并删)
```

安装完成后浏览器打开 `http://<目标机>:8787`,用安装摘要里打印的管理员账号登录。
可用环境变量覆盖默认值:`MC_CONSOLE_PORT` / `MC_AGENT_PORT` / `MC_ADMIN_PASSWORD` / `MC_DEPLOY_ROOTS` 等。

### 方式二:开发模式

```bash
# Console(前端 5173 热更新 + Go 后端 8787,/api 走 vite 代理)
cd console && pnpm install && pnpm dev

# Agent(另开一个终端)
cd agent && go run .
```

默认管理员:**admin / 1qaz@WSX**(仅在用户表为空时由 `config.toml` 的 `[admin]` 种入,bcrypt 存储;**对外监听前必须修改**,否则 Console 拒绝启动)。

### 单二进制构建

```bash
cd console && pnpm dist     # = vite build + go build -o mooncell .(前端已 go:embed)
./mooncell                  # http://localhost:8787
```

---

## ⚙️ 配置

两端各一个 `config.toml`。**默认监听 `127.0.0.1`(仅本机)**;改 `0.0.0.0` 对外监听前,**必须先改掉默认管理员密码与 Agent token**,否则 Console 会拒绝启动(纵深防御)。

**Console `console/config.toml`**

```toml
[server]
addr = "127.0.0.1"          # 对外请改 0.0.0.0,并务必先改下方默认口令/token
port = 8787
[database]
path = "mooncell.db"        # sqlite,首次运行自动创建
[admin]                     # 仅空库时种入(bcrypt);已有用户后改此处不影响既有账号
username = "admin"
password = "1qaz@WSX"
[agent]                     # Console 主动连接的默认 Agent
addr  = "127.0.0.1:9100"
token = "mc_ag_change_me"   # 须与 Agent 端一致
[deploy]
max_upload_mb = 1024        # 制品上传硬上限(传输层 MaxBytesReader 截断,超限回 413)
```

**Agent `agent/config.toml`**

```toml
[server]
addr = "127.0.0.1"
port = 9100
[security]
token = "mc_ag_change_me"   # 与 Console [agent].token 一致
[paths]
deploy_roots = ["/srv/apps", "/data/web"]   # 允许部署落盘的根目录白名单(防穿越)
log_roots    = ["/srv/apps", "/var/log"]    # 允许在线 tail 的日志根目录白名单
backup_dir   = "/opt/deploy-agent/backups"
```

---

## 📦 支持的部署类型与制品格式

Agent 按**文件魔数**(非扩展名)自动判断单文件 / 压缩包;压缩包**智能解包**:包内若只有单一顶层目录(`myapp-v1/…`)自动去掉该层,散落文件原样保留(过滤 `__MACOSX`/点文件)。

| 类型 | 制品格式 | 托管方式 |
|---|---|---|
| go-binary | 单个可执行二进制 | systemd / pm2,原子替换 |
| java-jar | 单个 `.jar` | systemd / pm2,原子替换 |
| python | 单文件 `.py` 或 压缩包(多文件 + `requirements.txt` 自动装依赖) | systemd / pm2,venv 解释器优先 |
| node | 单文件 `.js` 或 压缩包 | pm2 / systemd |
| static-nginx | 压缩包(`.tar.gz` / `.zip`) | 解包到带时间戳 release + **原子软链切换** |
| tomcat-war | 单个 `.war` | 原子替换 webapps 下 WAR,**容器自动展开** |

- 多文件应用备份/回滚为**整目录**(`app.tar.gz`),单文件为 `app`,还原按内容自动判断。
- 解包(tar.gz/zip)走 Go 标准库,**无需系统 `unzip`**;仅多文件应用的备份打包用到系统 `tar`。

---

## 🔒 安全说明

- **认证**:Console 用 bcrypt 校验口令 + httpOnly cookie 会话(过期自动失效);Agent 用共享 Token(`crypto/subtle` 常数时间比较)。
- **不下发任意 shell**:Agent 只暴露有限的类型化 API;reload 钩子只能选白名单内置动作(`nginx-reload` / `tomcat-restart`)。
- **路径白名单**:落盘/日志路径必须落在 `deploy_roots` / `log_roots` 内,用 `EvalSymlinks` 解析真实路径防软链逃逸。
- **压缩包安全解包**:逐条校验路径在目标内,拒绝绝对路径 / `..` 穿越 / 软硬链接(zip-slip),并限单文件大小 / 总量 / 条目数 / 目录深度防炸弹。
- **配置校验**:应用配置经类型化端点服务端校验(类型/Runner/路径/端口/范围/agentId)后才落库,前端无法绕过写脏配置。
- **不安全配置拒启**:对外监听(`0.0.0.0`)且仍用默认口令/token 时,Console 拒绝启动。

> 威胁模型:面向**可信内网**。admin/operator 本就能部署任意制品在目标机执行代码,平台不防御已授权运维;重点是防穿越、防注入、防误操作、防伪造记录。

---

## 🧪 开发与测试

```bash
# Go 单元测试(两端)
cd agent   && go test ./...
cd console && go test ./...

# 前端构建
cd console && pnpm build

# 前端 E2E(Playwright 无头浏览器驱动真实 Console + SQLite)
cd console && pnpm exec playwright install chromium   # 首次
cd console && pnpm test:e2e
```

测试覆盖:压缩包安全解包/超限、路径白名单与软链逃逸、幂等指纹冲突、还原源指纹、配置校验、能力过滤、日志失败错误态等;E2E 覆盖登录/会话/导航/能力置灰/预检一致性/失败态等关键前端流程。

---

## 🗂 目录结构

```
mooncell/
├── console/         Web 控制台(React + Tailwind 前端 + Go 后端,前端 go:embed)
│   ├── src/         前端源码(pages / components / lib)
│   ├── e2e/         Playwright E2E
│   └── *.go         登录/RBAC、Agent 代理、数据存储、分块上传、配置校验
├── agent/           目标机常驻服务(Go 单二进制)
│   └── *.go         Deployer / Runner / 备份 / 日志流 / 安全边界
├── deploy/          build-release.sh(打包) + install.sh(systemd 安装)
└── docs/            方案设计文档
```

---

## 🛣 路线图

已完成核心:六类 Deployer、systemd/pm2 双 Runner、备份还原、SSE 日志、多 Agent、RBAC、文件柜、分块上传、离线安装、服务端权威状态、真机(含 pm2/Tomcat 运行时)验收。

待增强(欢迎贡献):

- [ ] 审计/部署记录的保留策略与定期清理(当前为无界增长)
- [ ] 分块上传扩展到 Console→Agent 段
- [ ] 更细粒度的资源指标采集与告警
- [ ] Webhook / 定时部署 / 蓝绿发布

---

## 🤝 贡献

欢迎 Issue 与 PR。提交前请确保 `go test ./...`(两端)、`pnpm build` 与 `pnpm test:e2e` 通过。

## 📄 License

[MIT](LICENSE)
