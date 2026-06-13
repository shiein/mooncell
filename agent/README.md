# Mooncell Agent

部署目标机上的常驻服务,Go 单二进制、无运行时依赖。Console 通过 HTTP + 共享 Token 主动调用。

当前为 **P0 骨架**:Token 认证 + 启动能力自检 + 系统资源上报。真实 Deployer(部署落盘、进程起停、备份、日志流)按路线图后续实现。

## 运行

```bash
go run .                       # 读取 config.toml,默认监听 0.0.0.0:9100

# 生产:交叉编译为目标机单二进制(无 CGO)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o agent .   # UOS/麒麟 arm64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o agent .   # x86_64
```

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

## API(均需 `Authorization: Bearer <token>`)

| 方法 路径 | 说明 |
|---|---|
| `GET /api/ping` | 连通性测试:返回主机名、版本、OS/Arch、运行时长 |
| `GET /api/capabilities` | 能力清单:systemd / java / pm2 / nginx / python / node / tomcat 是否可用及版本 |
| `GET /api/system` | 资源水位:CPU / 内存 / 磁盘百分比与用量(gopsutil 采集) |
| `POST /api/apps/{id}/deploy` | 部署(go-binary):multipart 上传 `config`(JSON)+ `artifact`;同步跑流水线返回逐步结果 |
| `GET /api/apps/{id}/status` | systemd 托管状态:active / state / MainPID |
| `DELETE /api/apps/{id}` | 下线:停止 + 移除 unit(保留制品与备份) |

能力自检在启动时执行一次并缓存;Console 总览页据此过滤可选 Runner、绘制资源曲线与磁盘水位告警。

部署流水线(已在 Ubuntu 真机验证):校验 sha256 → 备份(滚动保留)→ 停止 → 原子 rename 替换 → 生成 systemd unit + 启动 → HTTP 健康检查(5×2s 重试)→ 失败自动回滚还原备份。制品路径经 `deploy_roots` 白名单校验(防穿越)。

## 待实现(路线图)

- **P0 剩余**:`static-nginx`(软链切换 + reload)与 `java-jar`(复用 systemd Runner + JRE)两个 Deployer;部署日志 SSE/WS 实时推送。
- **P1+**:应用日志 tail -F 实时流(fsnotify 处理轮转)+ 打包下载;pm2 Runner;tomcat-war / python Deployer;分块上传 + 断点续传。

详见 [../docs/deploy-platform-design-v1.md](../docs/deploy-platform-design-v1.md) §3 §4 §5 §8。
