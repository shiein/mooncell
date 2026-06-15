# Mooncell Agent

部署目标机上的常驻服务,Go 单二进制、无运行时依赖(纯 Go,CGO 关)。Console 通过 HTTP + 共享 Token 主动调用。

## 运行

```bash
go run .                       # 读取 config.toml,默认监听 0.0.0.0:9100

# 生产:交叉编译为目标机单二进制(无 CGO)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o agent .   # UOS/麒麟 arm64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o agent .   # x86_64
```

目标机依赖:`systemd`(进程类 Runner)、`tar`(自带)、`unzip`(zip 制品)、可选 `java`/`python3`/`node`/`pm2`(对应类型/Runner)。

## 配置 `config.toml`

```toml
[server]
addr = "0.0.0.0"
port = 9100

[security]
token = "mc_ag_change_me"      # Console 录入的共享 token,生产务必改成高熵随机值

[paths]                        # 安全边界:落盘/读日志路径规范化后须落在白名单根内(防穿越)
deploy_roots = ["/srv/apps", "/data/web"]
log_roots    = ["/srv/apps", "/var/log"]
backup_dir   = "/opt/deploy-agent/backups"
```

## Deployer / Runner

| 类型 | 制品形态 | Runner | 备份/回滚 |
|---|---|---|---|
| native-binary | 单二进制 | systemd / pm2 | 单文件 |
| java-jar | `.jar` | systemd / pm2 | 单文件 |
| python | 单文件 `.py` 或压缩包(多文件) | systemd / pm2 | 单文件 / 整目录 |
| node | 单文件 `.js` 或压缩包(多文件) | systemd / pm2 | 单文件 / 整目录 |
| static-nginx | 压缩包(tar.gz/zip) | 软链切换 | 历史 release 软链 |
| tomcat-war | `.war` | 容器托管 | 单文件 |

压缩包按**魔数**自动识别并智能解包:单一顶层目录自动去层,散落文件原样保留。
回滚连运行期配置(systemd unit / pm2 ecosystem)一起还原。进程类无 HTTP 健康检查时退化为查进程托管态(避免启动失败误判成功)。

## API(均需 `Authorization: Bearer <token>`)

| 方法 路径 | 说明 |
|---|---|
| `GET /api/ping` `GET /api/capabilities` `GET /api/system` | 连通性 / 能力清单 / 资源水位 |
| `POST /api/apps/{id}/deploy` · `…/deploy/stream` | 部署:multipart `config`(JSON)+ `artifact`;同步 / SSE 实时流。`config.expectedSha256` 非空则强校验制品 |
| `POST /api/apps/{id}/restore` · `…/restore/stream` | 还原:进程类用备份制品重跑流水线,static 切软链到历史 release |
| `GET /api/apps/{id}/backups` · `…/releases` | 列备份(进程类 BackupDir)/ 历史 release(static) |
| `GET /api/apps/{id}/status` | 托管状态(`?runner=pm2` 查 pm2) |
| `GET /api/apps/{id}/logs/stream` | 应用日志实时流(journal `-o json` / `?runner=pm2` 走 pm2 logs) |
| `GET /api/apps/{id}/logs/download` | 时间范围日志导出(`since`/`until` unix 秒,gzip attachment) |
| `GET /api/apps/{id}/logs/file/stream` | tail 声明的日志文件(`path` 经 log_roots 白名单校验) |
| `DELETE /api/apps/{id}` | 下线:停止 + 移除 unit / pm2(保留制品与备份) |

## 安全

- 所有落盘/读日志路径经 `deploy_roots` / `log_roots` 白名单校验(防穿越)。
- reload 钩子为白名单动作枚举(非自由 shell);systemd unit 字段拒绝换行/控制字符注入。
- `releaseId` 幂等:本地记录成功结果,重复请求直接返回缓存。
- 制品上传经 `[deploy] max_upload_mb` 传输层硬上限(默认 1024MB,超限 413),防超大制品撑爆磁盘。
- 多文件包整目录替换的目标(`Dir(binPath)`)不得为某个 `deploy_root` 本身,否则拒绝(防摧毁根下其它应用)。

### 日志文件 tail 的授权边界

- Agent 侧:`log_roots` 白名单是**穿越/越界**的硬边界(路径规范化后须落在白名单根内)。
- Console 侧:文件 tail 还要求路径属于该应用**已声明的 `logPaths`**(规范化后精确比对,仅绝对路径)。
  这道闸主要约束 **viewer(只读角色)**——viewer 不能改 `logPaths`,只能 tail 管理员/operator 声明过的日志。
- 对 admin/operator 不是提权:他们本就能通过部署任意制品在目标机执行代码读文件,声明 `logPath` 不构成新能力。
  因此 `log_roots` 应按需收窄(如无必要不要纳入 `/var/log` 全量)。

设计详见 [../docs/deploy-platform-design-v1.md](../docs/deploy-platform-design-v1.md);完成度见 [../README.md](../README.md)。
