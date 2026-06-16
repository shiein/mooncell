# Mooncell Agent

部署目标机上的常驻服务,Go 单二进制、无运行时依赖(纯 Go,CGO 关)。

## 编译

```bash
# 交叉编译为目标机单二进制(无 CGO)。-ldflags 注入版本号:供 Console 自更新比对与展示,
# 发布每个版本时把 vX.Y.Z 改成实际版本。
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.agentVersion=v0.2.0" -o agent .   # x86_64
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-X main.agentVersion=v0.2.0" -o agent .   # arm64(UOS/麒麟)

# 本机开发直接运行
go run .

# 自检 / 查版本(不启动服务)
./agent --version
./agent --selftest
```

启动:`nohup ./agent >agent.log 2>&1 &`。自更新走 self-exec 同 PID 重启,无需改启动方式。

## 配置 `config.toml`

```toml
[server]
addr = "0.0.0.0"
port = 9100

[security]
token = "mc_ag_change_me"      # 与 Console 录入一致的共享 token,生产务必改成高熵随机值

[paths]                        # 安全边界:落盘/读日志路径规范化后须落在白名单根内(防穿越)
deploy_roots = ["/srv/apps", "/data/web"]
log_roots    = ["/srv/apps", "/var/log"]
backup_dir   = "/opt/deploy-agent/backups"

[deploy]
max_upload_mb = 1024           # 部署制品上传传输层硬上限(MB),超限 413
```
