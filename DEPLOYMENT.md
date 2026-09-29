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

2026-09-29 发现 sing-box 1.12.21 的非复用 VLESS/VMess/Trojan TLS 和 WebSocket/HTTP Upgrade 拨号失败会丢失底层连接；HTTP/SOCKS 出站握手的阻塞读取也不响应 context 取消，真实 HTTP 节点曾使 CONNECT 建立持续约 81 秒。Resin 在这些分支按单次拨号记录原连接，失败或取消时释放，成功时移交原协议连接；当时复用传输及 WebSocket early-data 路径保留原有生命周期；同日后续 Early Data 修复已将其握手纳入连接跟踪，见下文。HTTP 出站拨号以及 CONNECT/SOCKS5 建立阶段采用最多 15 秒的预算（已有更短期限仍生效），预算覆盖早期握手和故障切换，不限制成功建立后的长流。根因、测试输出、镜像和每小时计数记录在本地 `.codex-artifacts/tls-close-fix-20260929/`。

宿主 Docker Backend 是共享进程，其 Bound/Handles 需结合容器内 TCP、连接创建时间和本轮新增连接判断。发布前遗留的固定 CLOSE_WAIT 不算新增泄漏。修复部署后计数从 0 开始，累计 12 次每小时有效观测；新增堆积或业务回归时停止计数并重新定位。

连接建立预算耗尽后立即结束本次请求，不再选择下一出口，避免将已过期请求的失败记到尚未尝试的节点。预算内的既有一次故障切换保持有效。

同日小时观测还发现周期探测饥饿：底层并发 Map 按固定桶顺序遍历，每轮达到小批量上限便退出；前部故障节点再次到期后反复占用配额，后部节点的健康信息长期未刷新。周期出口与延迟探测现先收集到期节点，优先探测最久未尝试的节点；出口配额在健康复验和熔断恢复之间轮流分配，单批仅一个名额时也跨轮交替。并发、每轮上限、启动速率、队列去重及原有到期条件保持不变；大池完整扫描仍需要时间，上游不可用不会因调度修复自动恢复。

此次发布前镜像保留为 `resin-local-rollback:probe-fairness-20260929`，ID 为 `sha256:4af2cc87435b3e38e268e2d369f4be5ea3c357fa376d53bccefca7dab0603305`。按前述回滚入口替换恢复标签可恢复上一个握手清理版本；公平调度的独立源码副本回滚和回归证据保存在 `.codex-artifacts/tls-close-fix-20260929/probe-fairness/`。在线镜像回滚尚未演练。

## 2026-09-29 订阅可用性修复

两条订阅存在不同代码缺陷，并叠加旧健康状态恢复缓慢：

- `node.codelove.cc.cd` 使用 VLESS + WebSocket Early Data。HTTP 拨号回调返回连接时提前取消了首次 Write 仍需使用的 context，出现 `operation was canceled`。现在在取消拨号上下文前完成延迟握手；将 WS Early Data 纳入原始连接跟踪，失败或超时关闭底层 socket。已建立流不受 15 秒建连预算限制，其他传输的延迟写入行为保留。
- “十年-github”的 47 个节点使用 VLESS XHTTP。部署前出口复测已恢复 47/47，故不能将恢复归因于 WS 补丁。实际 CONNECT 流量揭示另一个问题：客户端收到完整响应后关闭连接，HTTP/2 的私有 `http2: response body closed` 错误被当成失败，连续两次导致节点熔断。现在将该明确的本地 Body.Close 信号识别为正常关闭；无返回流量、异常 EOF 和流重置仍保持失败。
- 初始十年订阅有 20 个节点没有出口探测尝试记录。大池约 1.9 万节点，探测并发 4、每轮出口批量 2、每轮间隔 13–17 秒，恢复扫描较慢。本次通过有限并发复测刷新状态，未提高调度并发或改变订阅内容。

最终运行镜像：`sha256:354e6fe8aa905f9947e4d6f22805147a69ab77b1d708b4dea4d15b08b2e6c752`。源码基线 `f697d444630aa531b3d36cb719f2619be7500c60` 加本次未提交补丁。WebSocket 节点全量首轮 1659/1703 成功，其余 44 个分两轮重试后全部曾通过；两项修复最终部署后再抽样 32/32。XHTTP 最终镜像首轮 45/47，失败的 2 个重试均通过。最终 API 健康快照为 WS 1703/1703、XHTTP 47/47，该数字是实测时状态，后续会随网络和实际请求变化。

两种协议各通过 HTTP 204、HTTPS 204、HTTPS Cloudflare trace 200（含有效出口 IP）验证，实际路由哈希核对一致；6 条代理日志均 `net_ok=true`，关闭客户端后样本节点失败计数为 0、未熔断。临时平台已删除并核对原有平台集合。最终业务测前一次尝试有 1 个 XHTTP HTTP 请求 15 秒超时（502），随后复验成功；该轮证据保留，未记作成功。`/healthz=200`、`/readyz=200`、重启 0、OOM=false；原数据卷和其他 8 个容器 ID 均保持一致。

验证：默认 `go test ./... -count=1 -timeout=90s`、全部构建标签的 `go vet`、带全部标签的 outbound/netutil/proxy 专项测试均通过；专项测试排除了已有的两个 WireGuard 用例，分别涉及关闭阻塞及测试域名 NXDOMAIN，因此不声称所有带标签测试无条件通过。独立副本恢复 4 个生产文件后原回归重新失败，在线源码保持修复。

本次之前的镜像保留为 `resin-local-rollback:early-data-20260929`（`sha256:423704af7aa59a04878b1bac60422e690463f15cb6ffe2a172de4daabd676f71`）。本地恢复脚本 `.codex-artifacts/early-data-fix-20260929/ROLLBACK_DEPLOY.ps1` 校验镜像后仅重建 resin 并复用现有卷；该在线回滚入口未执行演练。`ROLLBACK.sh` 仅恢复隔离源码副本，已演练，不能等同于在线镜像恢复验证。完整补丁、原始哈希和证据位于 `.codex-artifacts/early-data-fix-20260929/`。

历史待整改：当前 Compose 使用本地可变镜像名 `resin-resin`，Windows 目录与 Linux `/opt/docker_projects` 规范不同。每次发布保存实际镜像 ID、源码哈希和原镜像恢复标签；下次部署方式迁移时复核固定镜像引用，保留本机目录和现有卷身份。Docker Desktop 重启影响其他项目，需要单独确认。
