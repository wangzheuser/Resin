# Resin Docker Compose 部署说明

## 快速启动

```bash
# 启动服务
docker compose up -d

# 查看日志
docker compose logs -f

# 查看状态
docker compose ps

# 停止服务
docker compose down
```

## 访问地址

- **管理界面**: http://127.0.0.1:9200/ui/platforms
- **健康检查**: http://127.0.0.1:9200/healthz
- **API 端点**: http://127.0.0.1:9200/api

## 认证信息

| 项目 | 值 |
|------|-----|
| 管理员 Token | `admin_secure_token_2026` |
| 代理 Token | `proxy_secure_token_2026` |
| 监听端口 | 9200 |

## 使用示例

### 正向代理

```bash
# 基本代理请求
curl -x http://127.0.0.1:9200 \
  -U "Default.proxy_secure_token_2026" \
  https://api.ipify.org

# 带 Platform 的请求
curl "http://127.0.0.1:9200/proxy_secure_token_2026/Default/https/api.ipify.org"
```

### 修改密码

编辑 `.env` 文件，修改以下变量：

```bash
RESIN_ADMIN_TOKEN=your-new-admin-token
RESIN_PROXY_TOKEN=your-new-proxy-token
```

然后重启服务：

```bash
docker compose restart
```

## 数据持久化

数据存储在以下 Docker 卷中：

- `resin_cache`: 缓存数据
- `resin_state`: 状态数据（订阅、平台配置等）
- `resin_log`: 日志文件

## 环境变量

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `RESIN_AUTH_VERSION` | 认证版本 | `V1` |
| `RESIN_ADMIN_TOKEN` | 管理员密码 | (需设置) |
| `RESIN_PROXY_TOKEN` | 代理密码 | (需设置) |
| `RESIN_PORT` | 服务端口 | `9200` |
| `TZ` | 时区 | `Asia/Shanghai` |

## 注意事项

1. 首次启动时会自动构建镜像，可能需要几分钟
2. GeoIP 数据库会在后台自动下载
3. 请妥善保管 Token，不要提交到版本控制
