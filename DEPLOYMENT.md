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

## 2026-09-29 订阅状态与限速复测

本次以 `e55f9ea9b17d0bc7b11e59ea15dfd0ee89218e76` 加工作区补丁构建，构建标识 `e55f9ea-subscription-health-local`，运行镜像为 `sha256:770172cab77801e94a9b657dd02cd7ef0f1b1743caeb3c5758dd1f444372d5f7`。补丁、源码哈希、实施方案及验证记录保存在 `.codex-artifacts/subscription-health-fix-20260929/`。

- 订阅响应保留原健康数，并新增待测、连接失败/熔断、构建失败、禁用、超过 24 小时未测及最近出口探测时间。健康/待测/失败/构建失败/禁用互斥覆盖总数，陈旧数独立计算。健康数仍表示缓存的可路由状态，受熔断阈值影响，可能与某轮复测成功数不同。页面将订阅更新状态明确标为拉取正常或解析正常。
- 普通 TLS 在出站构建时将不支持的 uTLS 指纹退回标准 TLS。支持集合对应固定的 sing-box 1.12.21；升级该依赖时复核。原始 JSON、节点 hash、证书验证、SNI、ALPN 和 Reality 严格校验保留。对已入库的 674 个 `unsafe` 节点，部署后 674/674 创建出站，原构建错误归零；这不表示它们均能联网。
- 新管理接口：`POST /api/v1/subscriptions/{id}/actions/probe-egress` 启动；`GET /api/v1/subscriptions/probe-job` 查询最近任务；`DELETE /api/v1/subscriptions/probe-job/{job_id}` 取消。接口使用既有管理员认证。全局仅一个活动订阅任务，重复启动同一订阅返回当前任务，另一个订阅返回 409。
- 复测逐项进入既有工作线程和启动限速器，保留当前并发 4、后台公平扫描、启动间隔及超时；未创建出站和已移除节点计为跳过。取消不增加节点失败计数。任务只在内存保留至下一任务或进程重启，不改变数据库结构。

验收：默认 Go 全量测试、部署标签专项测试及 go vet 通过，专项沿用跳过两个既有 WireGuard 环境用例；前端类型检查/构建、现有测试和变更文件 lint 通过。全站 lint 的既有其他页面错误记录在诊断目录，未作无关修整。独立源码副本执行 `ROLLBACK.sh` 后重新触发两项基线回归失败，主工作区保留修复；这是源码恢复验证，不是在线镜像回滚演练。

部署后全部订阅分类计数校验通过，任务重复启动、冲突、取消和完成均经过真实 API 验证。首轮 node3 为 3/20 成功，node2 为 27/64；同时旧辅助程序也出现 TCP 超时，旧/新路径均有成功与失败，不能将此前健康数或早先高成功率当成这一时刻的实际可用率。首轮代理业务验证失败已保留；随后 WebSocket、XHTTP 的 HTTP 204、HTTPS 204 和 Cloudflare trace 200（有效出口）共 6 项全部通过，实际路由节点核对一致，关闭客户端后失败计数为 0，临时平台已移除并核对原集合。

后续完整复验：node2 为 63/64，node3 从真实页面点击复测得到 20/20。浏览器完成进度、完成和取消操作验证，页面 JavaScript 错误为 0；实际页面入口为 `http://127.0.0.1:9200/ui/subscriptions`。以上是测试时结果，不保证后续上游可用率。

原数据卷、端口和其他容器身份保持不变。旧镜像保留为 `resin-local-rollback:subscription-health-20260929`，ID 为 `sha256:354e6fe8aa905f9947e4d6f22805147a69ab77b1d708b4dea4d15b08b2e6c752`。如需恢复旧程序，执行本地 `.codex-artifacts/subscription-health-fix-20260929/ROLLBACK_DEPLOY.ps1`，该入口校验旧镜像后仅重建 resin 并复用原卷，未在线执行回滚。

## 2026-09-29 订阅更新可靠性发布

当前镜像 `sha256:d1974f8023b34172e509b114df3daabb4bf189c0aa319161363434c9222ff4cc`，版本 `subscription-refresh-local`，基线 `e55f9ea` 加原有及本轮未提交修复。Compose 身份、三个卷、9200 端口保持原值。全局订阅下载代理由本机 `.env` 的 `RESIN_SUBSCRIPTION_PROXY_URL` 和覆盖文件注入，为 `http://host.docker.internal:7890`；普通节点前置平台配置独立。

统一刷新并发为全局 4、同来源主机 2。失败退避、真实错误响应及 cache.db 刷新历史已部署；默认 auto，页面可选 direct/platform/external_proxy。state.db 从 7 升到 8，cache.db 从 2 升到 3。

本地证据目录 `.codex-artifacts/subscription-refresh-fix-20260929/` 包含原始备份哈希、增量补丁、逐订阅结果、并发 UUID 真实负载和重启记录。首轮 56 个订阅中 55 个成功；free-cloud 上游 DNS/TLS/连接异常保留缓存。首轮后观察至少 20 分钟，随后正常重启验证历史精确恢复。动态 UUID 两轮 857 次中 735 次成功，当前出口池未达到 300 次/分钟目标；readyz 曾因健康出口 IP 比例低于 20% 返回 503，healthz 正常，保留该限制。

回滚旧镜像标签为 `resin-local-rollback:subscription-refresh-20260929`。本次结构迁移后，恢复发布前版本必须先降级数据库，不能仅替换镜像。执行 `python .codex-artifacts/subscription-refresh-fix-20260929/ROLLBACK_DEPLOY.py --check` 完成预检；执行同入口 `--apply` 会仅停止 Resin，导出新增下载配置、刷新历史和完整数据库备份，撤销本次结构，恢复 state 7/cache 2 后启动原镜像。真实数据库副本往返迁移、旧镜像隔离启动已通过，在线回退未执行。

## 2026-09-30 动态 UUID 代理可靠性发布

本轮版本 `proxy-reliability-local`，源码 `e55f9ea-dirty`，镜像 `sha256:c1b88d32ac67ae0e0ecf705f709551c6eacfba7e67f62c424fb56028bf523091`。保留订阅可靠性、节点兼容性和资源清理修复。只重建本机 `resin`，继续从项目工作目录读取 `.env`、`docker-compose.yml`、`docker-compose.override.yml`（项目 `resin`），三个外部卷、9200 端口和 resin_default 网络沿用原值。

- CONNECT/SOCKS5 共用的建连流程仍受总 15 秒预算限制，首次出口使用最多 7.5 秒，留出第二个不同出口的尝试机会。预算覆盖延迟协议握手，成功建立后的流不受该预算影响，整体截止后不会选择或惩罚未尝试节点。
- 普通 HTTP 在 GET/HEAD/OPTIONS/TRACE 无请求体、尚未取得连接时允许一次换出口，单次建连最多 8 秒。已取得连接（可能已部分写入）、带请求体和写操作不重放；成功响应及长流不被建连计时器截断。沿用出口排除、健康反馈及故障切换指标和日志。
- 出口探测在解析出有效 IP 后才清零失败次数、解除熔断、更新成功延迟；HTTP 成功但内容为错误页面不再恢复旧节点。

这些变化不修改订阅原文、节点 hash、前置平台或探测并发配置。temp-1/temp-2 使用 github 平台前置连接，端到端结果受前置节点、末端 HTTP 节点和目标三者影响。仅看 HTTP 类型或宿主直连结果不足以单独归因到末端节点。

证据位于 `.codex-artifacts/proxy-reliability-fix-20260930/`。`REPORT.md`、`VERIFICATION.txt` 和逐次 `load-*.json` 是本轮真实负载、20 分钟观测与正常重启的最终记录；既有 healthz 正常、readyz 因健康出口比例不足返回 503 的区别仍保留。源码行为回归为旧版失败、修复通过、隔离副本回滚后恢复旧行为；后端完整测试、部署标签专项测试、vet 和 Linux proxy/probe race 通过。

本轮没有数据库迁移，保持 state 8/cache 3。恢复入口为 `python .codex-artifacts/proxy-reliability-fix-20260930/ROLLBACK_DEPLOY.py --check`（只读预检），需要恢复本轮发布前镜像时使用同入口 `--execute`。原镜像保存为 `resin-local-rollback:proxy-reliability-20260930`（`sha256:d1974f8023b34172e509b114df3daabb4bf189c0aa319161363434c9222ff4cc`），此回退不执行上一轮 state 7/cache 2 降级。SQLite 一致性备份及原始配置保存在受限的本轮 backups 目录。源码副本回滚已测试，在线镜像回滚未演练。

## 2026-09-30 最小化根治修复与高频代理复验

本轮针对启动刷新风暴、周期刷新阻塞、错误订阅响应被当作成功、Cloudflare trace 解析失败误伤节点，以及订阅列表默认暴露本地原文的问题做最小化修复。周期刷新统一异步入队并按持久化到期时间执行，移除启动时无条件全量刷新；下载响应限制为 16 MiB，auto 出口对 HTTP 200 内容做有效订阅校验后再切换出口；Cloudflare trace 解析失败不增加节点失败计数并保留最近有效出口 IP；订阅列表/详情默认不返回本地订阅原文，详情需要显式 `include_content=true`。

后端 `go test ./... -count=1 -timeout=180s`、`go vet ./...` 通过；前端在 `webui` 执行 `npm run build` 和 `npm test`（3/3）通过。全站 ESLint 仍有既有的 `PlatformMonitorPanel.tsx` 与 `RequestLogsPage.tsx` 问题，本轮未改动；本机 race 重跑因未安装 gcc 未执行。

部署前给旧镜像 `sha256:c1b88d32ac67ae0e0ecf705f709551c6eacfba7e67f62c424fb56028bf523091` 添加恢复标签 `resin-local-rollback:minimal-rootfix-20260930`，随后执行 `docker compose build resin` 和 `docker compose up -d --no-deps --force-recreate resin`。新镜像为 `sha256:6a56209a93070a68995528e46d313c59ae33660eebf958b7a0f2b26fbd4270fb`；三个外部卷、9200 端口和其他容器保持原运行身份。

真实验证：并发 5、动态 UUID 300 次通过 `http://node.{uuid}:<redacted>@127.0.0.1:9200` 请求 Cloudflare trace，289 次 HTTP 200，11 次失败（2 次超时、9 次其他上游/网络错误），总耗时 129447 ms；同一订阅并发刷新 5 次全部 200。服务保持 healthy、无重启、无 OOM。15 分钟 15 个采样点均为 running/healthy，内存 194.6–198.2 MiB，TIME_WAIT=0，panic/fatal/OOM/SQLite locked=0。

订阅状态保持 56 条，其中 55 条 `ok`、1 条 `retry_wait`；缓存节点总数 26574。`free-cloud` 仍保留 41 个缓存节点并展示上游错误，不被误报为成功。默认订阅列表和详情的 `content` 长度均为 0。正常重启后加载 12778 个节点动态状态和 10797 条延迟记录，订阅状态及缓存节点数量不变；重启后额外 5/5 动态 UUID 请求返回 200。`/healthz=200`；`/readyz=503` 仍是健康出口比例不足的既有条件。

回滚入口：恢复前先确认 `resin-local-rollback:minimal-rootfix-20260930`，再执行 `docker tag resin-local-rollback:minimal-rootfix-20260930 resin-resin` 和 `docker compose up -d --no-deps --force-recreate --no-build resin`。本轮没有执行在线回滚；源码副本回滚已由 `.codex-artifacts/minimal-rootfix-20260930/ROLLBACK.sh` 验证。完整命令、输入、字面输出和哈希位于 `.codex-artifacts/minimal-rootfix-20260930/VERIFICATION.txt`。
