# Mooncell

> 面向**内网 / 单机 / 离线**交付环境的轻量自动化部署平台 —— 上传即部署、自动备份、一键还原、在线看日志。

Mooncell 用两个**零依赖的 Go 单二进制**(Console 控制台 + Agent 部署代理)取代手工 `scp + systemctl` 的部署方式。前端经 `go:embed` 编进 Console 二进制,数据存纯 Go 的 SQLite,**无需 Docker、无需运行时依赖**,特别适合 UOS / 麒麟等国产化、离线、内网场景。

## ✨ 特性

- **多种部署类型**:go-binary / java-jar / python / node(systemd / pm2 托管)+ static-nginx(软链原子切换)+ tomcat-war(容器换 WAR)。
- **上传即部署**:浏览器上传制品 → 自动流水线(备份 → 停 → 替换 → 启动 → 健康检查),**失败自动回滚**。
- **一键还原**:列历史备份,用任意历史制品重跑部署流水线;还原前自动备份当前版本。
- **实时日志**:journald / pm2 logs / 声明文件 `tail -F` 经 SSE 推到前端,支持暂停、高亮、gzip 下载。
- **多 Agent**:一台 Console 管多台目标机,应用按 `agentId` 路由;按 Agent 真实能力过滤可选 Runner。
- **分块上传 + 断点续传**:大制品自动分块,网络中断可续传。
- **安全可控**:RBAC、bcrypt + httpOnly 会话、共享 Token、路径白名单、压缩包安全解包、Agent 只暴露类型化 API(无任意 shell)。
- **离线交付**:`install.sh` 一键把两端装为 systemd 服务、自动生成共享 Token 打通。

## 🏗 架构

```
Console(Go 单二进制：React go:embed + SQLite)
      │ HTTP + 共享 Token(SSE 流式日志/部署)
      ▼
Agent(Go 单二进制，目标机：Deployer / Runner / 备份 / 日志流)
      ▼
systemd / pm2 / Tomcat 容器 / nginx 目录
```

单机可同机部署;架构上一台 Console 天然支持管多台 Agent。设计细节见 [docs/deploy-platform-design-v1.md](docs/deploy-platform-design-v1.md)。

## 🚀 快速开始

### 离线安装(推荐,生产)

```bash
# 1) 构建机(需 go + pnpm):产出离线 bundle
ARCH=amd64 deploy/build-release.sh           # 国产化 arm64:ARCH=arm64
# → deploy/release/mooncell-amd64/{mooncell-console, mooncell-agent, install.sh}

# 2) 目标机(root):拷入整个 bundle 目录后
./install.sh install        # 装为 systemd 服务,自动生成共享 Token、打印初始管理员口令
./install.sh status         # 查看服务状态与访问地址
./install.sh upgrade        # 仅换二进制并重启,保留配置与数据库
./install.sh uninstall      # 卸载(默认留数据;--purge 连数据一并删)
```

安装完成后浏览器打开 `http://<目标机>:8787` 登录。

### 开发模式

```bash
cd console && pnpm install && pnpm dev    # 前端 5173 + Go 后端 8787
cd agent   && go run .                     # 另开终端,Agent 监听 9100
```

默认管理员:**admin / 1qaz@WSX**(仅空库首启种入;**对外监听前必须修改**,否则 Console 拒绝启动)。

## ⚙️ 配置

两端各一个 `config.toml`。**默认监听 `127.0.0.1`(仅本机)**;改 `0.0.0.0` 对外前必须先改默认口令与 Agent token,否则拒启。

**Console**

```toml
[server]
addr = "127.0.0.1"          # 对外请改 0.0.0.0,并先改下方默认口令/token
port = 8787
[admin]
username = "admin"
password = "1qaz@WSX"
[agent]
addr  = "127.0.0.1:9100"
token = "mc_ag_change_me"   # 须与 Agent 端一致
```

**Agent**

```toml
[server]
addr = "127.0.0.1"
port = 9100
[security]
token = "mc_ag_change_me"
[paths]
deploy_roots = ["/srv/apps", "/data/web"]   # 允许部署落盘的根目录白名单(防穿越)
log_roots    = ["/srv/apps", "/var/log"]    # 允许在线 tail 的日志根目录白名单
backup_dir   = "/opt/deploy-agent/backups"
```

## 📦 部署类型与制品格式

Agent 按**文件魔数**自动判断单文件 / 压缩包;压缩包智能解包(单一顶层目录自动去层)。

| 类型 | 制品格式 | 托管方式 |
|---|---|---|
| go-binary | 单个可执行二进制 | systemd / pm2,原子替换 |
| java-jar | 单个 `.jar` | systemd / pm2,原子替换 |
| python | `.py` 或 压缩包(多文件 + `requirements.txt` 自动装依赖) | systemd / pm2,venv 优先 |
| node | `.js` 或 压缩包 | pm2 / systemd |
| static-nginx | 压缩包(`.tar.gz` / `.zip`) | 解包到带时间戳 release + 原子软链切换 |
| tomcat-war | 单个 `.war` | 原子替换 webapps 下 WAR,容器自动展开 |

## 📄 License

[MIT](LICENSE)
