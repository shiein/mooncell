# Mooncell

> 面向**内网 / 单机 / 离线**交付环境的轻量自动化部署平台 —— 上传即部署、自动备份、一键还原、在线看日志。

Mooncell 用两个**零依赖的 Go 单二进制**(Console 控制台 + Agent 部署代理)取代手工 `scp + systemctl` 的部署方式。前端经 `go:embed` 编进 Console 二进制,数据存纯 Go 的 SQLite,**无需 Docker、无需运行时依赖**,特别适合 UOS / 麒麟等国产化、离线、内网场景。

## ✨ 特性

- **多种部署类型**:原生二进制(Go / Rust / C++ / Zig 等为目标机编译的可执行文件)/ java-jar / python / node(systemd / pm2 托管)+ static-nginx(软链原子切换)+ tomcat-war / tongweb-war(容器换 WAR)。
- **上传即部署**:浏览器上传制品 → 自动流水线(备份 → 停 → 替换 → 启动 → 健康检查),**失败自动回滚**。
- **一键还原**:列历史备份,用任意历史制品重跑部署流水线;还原前自动备份当前版本。
- **实时日志**:journald / pm2 logs / 声明文件 `tail -F` 经 SSE 推到前端,支持暂停、高亮、gzip 下载。
- **多 Agent**:一台 Console 管多台目标机,应用按 `agentId` 路由;按 Agent 真实能力过滤可选 Runner。
- **分块上传 + 断点续传**:大制品自动分块,网络中断可续传。
- **Console / Agent 自更新**:管理员从浏览器上传新二进制,本地校验 sha256 + ELF 架构 + `--selftest` + `--version` 后替换自身并 `syscall.Exec` 就地重启(同 PID,适配 nohup 无监管场景);Console 二进制 `go:embed` 内嵌前端,换一个二进制 = 前后端一起升级。失败保持旧版,留 `<exe>.old` 供手工回滚。
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

### 源码结构

```text
agent/
├── main.go                 # Agent 可执行入口与版本注入点
└── internal/agentapp/      # Agent 现有应用实现及同包测试
console/
├── main.go                 # Console 可执行入口、前端嵌入与版本注入点
├── internal/consoleapp/    # Console 现有应用实现及同包测试
└── src/                    # React 前端（pages/components/lib）
docs/                       # 设计与开发文档
deploy/                     # 构建、安装与升级脚本
```

根目录只保留稳定入口和构建文件。后续新增具备独立模型、权限或外部依赖的领域功能时放入对应的 `internal/<domain>` 包，由入口应用组装；不为文件数量而提前拆分现有调用链。

## 🚀 快速开始

### 离线安装(推荐,生产)

```bash
# 1) 构建机(需 go + pnpm):产出离线 bundle
ARCH=amd64 deploy/build-release.sh           # 国产化 arm64:ARCH=arm64
# → deploy/release/mooncell-amd64/{mooncell-console, mooncell-agent, install.sh}

# 2) 目标机(root):拷入整个 bundle 目录后
./install.sh install        # 装为 systemd 服务,自动生成共享 Token、打印初始管理员口令
./install.sh status         # 查看服务状态与访问地址
./install.sh upgrade        # 停机 → 自动快照数据库+凭据密钥 → 换二进制 → 重启
./install.sh backup         # 手工快照 mooncell.db + mooncell-etl.key + config 到 backups/<时间戳>/
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

## ⚠️ 约束与边界

- **Console 须单实例运行**:健康巡检的「在飞操作引用计数」(busy 集)、分块上传会话、远端 Agent 客户端缓存均为**进程内状态**,SQLite 也以 `MaxOpenConns=1` 单连接串行写入。多实例 Console 并存时这些进程内状态会失效(例如 A 实例正在部署的应用,B 实例的巡检可能误判掉线;两实例并发改同一应用实体存在 read-modify-write 覆盖风险)。当前设计面向单实例交付,如需多实例需引入外部协调(共享 busy 存储 + 行锁),暂不在支持范围。
- **Agent 可多机、每机单实例**:一台 Console 管多台 Agent 是支持的;但同一目标机的 Agent 应只跑一个实例(共用同一 `config.toml` 与端口)。

## 🔄 数据集成(ETL)

Console 内置可视化数据集成:画布拖拽编排**转换**(输入/清洗/映射/排序/输出)与**作业**(转换+HTTP/子作业/通知),支持定时调度、运行历史与日志、版本快照续跑、定义导入导出。设计细节见 [docs/etl-design-v1.md](docs/etl-design-v1.md)。

- **连接**:支持 PostgreSQL / MySQL / SQLite / 达梦 / 人大金仓;连接密码 AES-GCM 加密落库。
- **授权**:管理员在「用户」页按定义授 `use`(查看/运行/导出)或 `edit`(可改/调度);零授权用户看不到该模块。连接管理与数据预览仍为管理员/编辑者能力。
- **配置**(`console/config.toml`):

```toml
[etl]
enabled = true                # 关闭则整个模块不出现在导航与 API
key_file = ""                 # 凭据密钥路径,缺省与数据库同目录 mooncell-etl.key
file_root = ""                # csv_input/csv_output 的相对路径根目录,缺省与数据库同目录 etl-files(0750)
max_concurrent_runs = 4       # 同时运行的作业数上限
retention_days = 30           # 运行记录保留天数
```

- **文件边界**:csv_input / csv_output 的 `path` 一律解释为 `file_root` 下的相对路径——绝对路径、`..` 越界、指向根外的符号链接在编译期与运行期都被拒绝,对管理员同样生效。需要读写服务器其它目录时,把 `file_root` 指向该目录,或把数据复制/挂载进 `file_root`。
- **源端下推**(转换设置 → 源端下推,默认关):开启后「表输入 → 过滤 → 字段选择」折叠为源库 SQL,但只下推能证明与行内求值等价的条件子集(数值/日期/时间戳的比较与 in、`!=`/`not in` 补 NULL 语义、NULL 谓词);字符串比较、函数、`var()`、三值逻辑会改变行集合的结构一律留在行内执行。字面量全部走绑定参数,不内联进 SQL。**预览永不下推**——预览展示的是行内求值结果,与开启与否无关。

- **备份与升级(重要)**:`mooncell-etl.key` 是库内全部连接密码与通知凭据的加密密钥,**必须与 `mooncell.db` 成对备份**——只备份数据库、丢失密钥,所有已保存密码无法解密,需逐个连接重输。`install.sh upgrade` 已自动快照二者到 `$PREFIX/backups/pre-upgrade-<时间戳>/`;手工迁移/灾备请用 `./install.sh backup`,并确认备份介质中密钥与库文件同批保管。
- **迁移**:定义可经「工程列表 → 导出/导入」跨实例迁移,连接引用按名称自动匹配;凭据明文永不导出,导入后需重填。

## � 自更新(Console / Agent)

为离线 / 内网 / 单机 nohup 部署场景设计——无需 ssh、无需 systemd 守护,管理员在浏览器里就能升级两端:

- **Console 自更新**:登录 admin → 「系统」页 → 上传新 `mooncell-console` 二进制。Console 落临时文件并校验 `sha256` + ELF 架构匹配本机 + `--selftest`(只 `loadConfig`,不绑端口/不开 DB) + `--version` 后,备份当前为 `<exe>.old`、原子 `os.Rename` 替换自身,延迟 500ms `syscall.Exec` 用新二进制就地重启(同 PID,端口在新映像启动时重新 bind)。前端轮询 `/api/console/info` 直到版本变为新版即确认重启完成。Console 二进制 `//go:embed all:dist` 内嵌前端,故换一个二进制 = 前后端一起升级。
- **Agent 自更新**:在「Agent 管理」页按架构(linux amd64/arm64)上传一次 agent 二进制,Console 把匹配包推送到各 Agent 的 `/api/self-update`,Agent 端同款校验链 + self-exec 重启。
- **校验链 fail-closed**:任一校验不过即保持旧版、删 `<exe>.new`,无损。允许同版本覆盖(管理员常忘记改版本号)。
- **回滚**:`<exe>.old` 留在二进制旁,新版起不来时手工 `mv <exe>.old <exe>` 重启即可。自更新前建议先备份 `mooncell.db`(跨版本迁移风险)。
- **边界**:仅接受匹配本机 `runtime.GOARCH` 的 linux ELF;darwin 上跑的 Console(开发机)会拒绝任何上传(自更新是 Linux 部署特性)。重启瞬间约 0.5–2s 断连,在飞操作(部署/还原 SSE、分块上传会话、巡检)会被切断,建议在空闲窗口升级。

## �📦 部署类型与制品格式

Agent 按**文件魔数**自动判断单文件 / 压缩包;压缩包智能解包(单一顶层目录自动去层)。

| 类型 | 制品格式 | 托管方式 |
|---|---|---|
| 原生二进制(`native-binary`) | 单个可执行文件 —— **任意语言**(Go/Rust/C++/Zig…)**为目标机 OS/架构编译**的自包含二进制;部署前校验 ELF 架构匹配、拦截 Mach-O/PE | systemd / pm2,原子替换 |
| java-jar | 单个 `.jar` | systemd / pm2,原子替换 |
| python | `.py` 或 压缩包(多文件 + `requirements.txt` 自动装依赖) | systemd / pm2,venv 优先 |
| node | `.js` 或 压缩包 | pm2 / systemd |
| static-nginx | 压缩包(`.tar.gz` / `.zip`) | 解包到带时间戳 release + 原子软链切换 |
| tomcat-war | 单个 `.war` | 原子替换 webapps 下 WAR,容器自动展开 |
| tongweb-war | 单个 `.war` | 原子替换 TongWeb `deployment` 下指定 WAR；不重启共享容器，等待自动部署并强制 HTTP 探活 |

### TongWeb 部署

前端、后端 WAR 按两个独立应用管理，分别填写各自完整目标路径（例如
`/opt/TongWeb/deployment/frontend.war`、`/opt/TongWeb/deployment/backend.war`）和 HTTP 健康检查 URL。
Agent 的 `deploy_roots` 必须包含实际 `deployment` 目录，运行用户必须能在该目录创建临时文件、原子
`rename`，并在覆盖旧 WAR 时保留其 owner/group。流水线为：校验 WAR 结构与 CRC → 备份当前 WAR →
保留权限原子替换 → 等待 TongWeb 自动部署 → HTTP 2xx 探活（重定向不算通过）；失败时自动还原旧 WAR。删除 Mooncell 应用
只注销管理记录，保留共享 TongWeb 容器和在役 WAR。

## 📄 License

[MIT](LICENSE)
