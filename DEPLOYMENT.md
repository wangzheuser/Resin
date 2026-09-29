# 本机 Resin Docker 部署

核查日期：2026-09-29。工作目录为 `E:/projects/github_projects/Resin`，Compose 项目名和服务名均为 `resin`。

## 操作入口

在上述目录使用 PowerShell，Compose 自动按顺序读取 `docker-compose.yml`、本机 `docker-compose.override.yml`，变量来自本机 `.env`。

```powershell
# 配置检查（不输出秘密）
docker compose config --quiet
# 状态查询
docker compose ps resin
docker inspect resin --format '{{.State.Status}}|{{.State.Health.Status}}|restart={{.RestartCount}}|oom={{.State.OOMKilled}}|image={{.Image}}'
# 版本发布：先记录旧镜像 ID 并添加本地恢复标签，然后构建和更新目标服务
docker compose build resin
docker compose up -d --no-deps resin
# 活性检查
curl.exe --noproxy '*' --fail http://127.0.0.1:9200/healthz
```

监听端口保持宿主 `0.0.0.0:9200` / `[::]:9200`。这是现有局域网代理入口的业务例外，本次不调整暴露和防火墙；下次修改监听地址或访问范围时复核。

## 数据与恢复

本机覆盖文件绑定既有外部卷，部署前后须确认名称一致：

| 卷 | 容器路径 |
| --- | --- |
| `resin_restore_20260831_163016_cache` | `/var/cache/resin` |
| `resin_restore_20260831_163016_state` | `/var/lib/resin` |
| `resin_restore_20260831_163016_log` | `/var/log/resin` |

`.env`、覆盖文件和运行数据保持本地，不进入提交。程序回滚复用原数据卷，不运行 `down -v`。

2026-09-29 修复前镜像保留为 `resin-local-rollback:tls-close-20260929`，ID 为 `sha256:f1bb3303187f1cd5d7a5f9889ea2af70485a42c805e06cac983e0af3833af187`。恢复旧程序入口：

```powershell
docker image inspect resin-local-rollback:tls-close-20260929 --format '{{.Id}}'
docker tag resin-local-rollback:tls-close-20260929 resin-resin
docker compose up -d --no-deps --no-build --force-recreate resin
```

此入口未在在线服务执行回滚演练。独立副本已验证源码恢复会重新触发握手泄漏测试失败；不能将源码副本恢复等同于在线业务恢复。

## 验收与观测

每次发布检查容器健康、重启次数、OOM、日志，以及带新 UUID 的真实代理请求；凭据由本机 `.env` 读入，不能写入测试输出。`/healthz=200` 表示进程存活，`/readyz` 还检查健康出口比例；2026-09-29 的就绪基线为 503（健康节点比例低于 10%），不能据此宣称所有出口可用。

2026-09-29 发现 sing-box 1.12.21 的非复用 VLESS/VMess/Trojan TLS 和 WebSocket/HTTP Upgrade 拨号失败会丢失底层连接；HTTP/SOCKS 出站握手的阻塞读取也不响应 context 取消，真实 HTTP 节点曾使 CONNECT 建立持续约 81 秒。Resin 在这些分支按单次拨号记录原连接，失败或取消时释放，成功时移交原协议连接；复用传输及 WebSocket early-data 路径保留原有生命周期。HTTP 出站拨号以及 CONNECT/SOCKS5 建立阶段采用最多 15 秒的预算（已有更短期限仍生效），预算覆盖早期握手和故障切换，不限制成功建立后的长流。根因、测试输出、镜像和每小时计数记录在本地 `.codex-artifacts/tls-close-fix-20260929/`。

宿主 Docker Backend 是共享进程，其 Bound/Handles 需结合容器内 TCP、连接创建时间和本轮新增连接判断。发布前遗留的固定 CLOSE_WAIT 不算新增泄漏。修复部署后计数从 0 开始，累计 12 次每小时有效观测；新增堆积或业务回归时停止计数并重新定位。

连接建立预算耗尽后立即结束本次请求，不再选择下一出口，避免将已过期请求的失败记到尚未尝试的节点。预算内的既有一次故障切换保持有效。

历史待整改：当前 Compose 使用本地可变镜像名 `resin-resin`，Windows 目录与 Linux `/opt/docker_projects` 规范不同。每次发布保存实际镜像 ID、源码哈希和原镜像恢复标签；下次部署方式迁移时复核固定镜像引用，保留本机目录和现有卷身份。Docker Desktop 重启影响其他项目，需要单独确认。
