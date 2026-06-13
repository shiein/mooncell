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

能力自检在启动时执行一次并缓存;Console 总览页据此过滤可选 Runner、绘制资源曲线与磁盘水位告警。

## 待实现(路线图)

- **P0 剩余**:`java-jar`(nohup/systemd)与 `static-nginx` 两个 Deployer;上传(分块 + sha256)→ 部署流水线 → 健康检查闭环;部署日志 SSE/WS。
- **P1+**:自动备份 + 一键还原;应用日志 tail -F 实时流(fsnotify 处理轮转)+ 打包下载;pm2 Runner;tomcat-war / go-binary / python Deployer。

详见 [../docs/deploy-platform-design-v1.md](../docs/deploy-platform-design-v1.md) §3 §4 §5 §8。
