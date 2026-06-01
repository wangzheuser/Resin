#!/bin/bash
# Resin 启动脚本 (macOS/Linux)
# 自动检测环境、安装依赖、构建并启动服务

set -e

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# 日志函数
log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[OK]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# 项目根目录
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$PROJECT_ROOT"

# ==================== 环境检测 ====================

detect_os() {
    case "$(uname -s)" in
        Linux*)     OS="linux";;
        Darwin*)    OS="macos";;
        *)          log_error "不支持的操作系统: $(uname -s)"; exit 1;;
    esac
    log_success "操作系统: $OS"
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)   ARCH="amd64";;
        arm64|aarch64)   ARCH="arm64";;
        *)               log_error "不支持的架构: $(uname -m)"; exit 1;;
    esac
    log_success "架构: $ARCH"
}

# 检查命令是否存在
check_command() {
    command -v "$1" &> /dev/null
}

# 版本比较函数 (返回 0 如果 $1 >= $2)
version_gte() {
    [ "$(printf '%s\n' "$1" "$2" | sort -V | head -n1)" = "$2" ]
}

check_go() {
    log_info "检查 Go 环境..."

    if ! check_command go; then
        log_error "未检测到 Go 环境"
        echo ""
        echo "请安装 Go 1.25+:"
        echo "  macOS:  brew install go"
        echo "  Linux:  https://go.dev/doc/install"
        echo ""
        exit 1
    fi

    GO_VERSION=$(go version | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' | head -1)
    GO_MAJOR_MINOR=$(echo "$GO_VERSION" | cut -d. -f1,2)

    if ! version_gte "$GO_VERSION" "1.25.0"; then
        log_error "Go 版本过低: $GO_VERSION (需要 >= 1.25)"
        echo ""
        echo "请升级 Go:"
        echo "  macOS:  brew upgrade go"
        echo "  Linux:  https://go.dev/doc/install"
        echo ""
        exit 1
    fi

    log_success "Go 版本: $GO_VERSION"
}

check_node() {
    log_info "检查 Node.js 环境..."

    if ! check_command node; then
        log_error "未检测到 Node.js 环境"
        echo ""
        echo "请安装 Node.js 22+:"
        echo "  macOS:  brew install node@22"
        echo "  Linux:  https://nodejs.org/"
        echo "  或使用 nvm: nvm install 22"
        echo ""
        exit 1
    fi

    NODE_VERSION=$(node -v | sed 's/^v//')
    NODE_MAJOR=$(echo "$NODE_VERSION" | cut -d. -f1)

    if [ "$NODE_MAJOR" -lt 22 ]; then
        log_error "Node.js 版本过低: v$NODE_VERSION (需要 >= 22)"
        echo ""
        echo "请升级 Node.js:"
        echo "  macOS:  brew upgrade node"
        echo "  或使用 nvm: nvm install 22 && nvm use 22"
        echo ""
        exit 1
    fi

    log_success "Node.js 版本: v$NODE_VERSION"

    # 检查 npm
    if ! check_command npm; then
        log_error "未检测到 npm"
        exit 1
    fi

    NPM_VERSION=$(npm -v)
    log_success "npm 版本: $NPM_VERSION"
}

# ==================== 环境变量处理 ====================

parse_env_file() {
    local env_file="$1"

    if [ ! -f "$env_file" ]; then
        return 1
    fi

    # 读取 .env 文件，跳过注释和空行
    while IFS='=' read -r key value; do
        # 跳过注释和空行
        [[ "$key" =~ ^#.*$ || -z "$key" ]] && continue
        # 移除首尾空格
        key=$(echo "$key" | xargs)
        value=$(echo "$value" | xargs)
        # 移除引号
        value="${value#\"}"
        value="${value%\"}"
        value="${value#\'}"
        value="${value%\'}"
        # 导出变量
        export "$key=$value"
    done < "$env_file"

    return 0
}

load_env() {
    log_info "加载环境变量..."

    # 尝试从 .env 文件加载
    if parse_env_file "$PROJECT_ROOT/.env"; then
        log_success "从 .env 文件加载配置"
    else
        log_warn "未找到 .env 文件"
    fi

    # 检查必需的 token
    if [ -z "$RESIN_ADMIN_TOKEN" ]; then
        echo ""
        log_warn "未配置 RESIN_ADMIN_TOKEN"
        read -p "请输入管理员 Token: " RESIN_ADMIN_TOKEN
        if [ -z "$RESIN_ADMIN_TOKEN" ]; then
            log_error "RESIN_ADMIN_TOKEN 不能为空"
            exit 1
        fi
        export RESIN_ADMIN_TOKEN
    else
        log_success "RESIN_ADMIN_TOKEN: ${RESIN_ADMIN_TOKEN:0:8}..."
    fi

    if [ -z "$RESIN_PROXY_TOKEN" ]; then
        echo ""
        log_warn "未配置 RESIN_PROXY_TOKEN"
        read -p "请输入代理 Token (留空表示无密码): " RESIN_PROXY_TOKEN
        export RESIN_PROXY_TOKEN
    else
        log_success "RESIN_PROXY_TOKEN: ${RESIN_PROXY_TOKEN:0:8}..."
    fi

    # 设置默认值
    export RESIN_AUTH_VERSION="V1"
    export RESIN_PORT="${RESIN_PORT:-2260}"
    export RESIN_LISTEN_ADDRESS="${RESIN_LISTEN_ADDRESS:-127.0.0.1}"
    export RESIN_STATE_DIR="${RESIN_STATE_DIR:-$PROJECT_ROOT/data/state}"
    export RESIN_CACHE_DIR="${RESIN_CACHE_DIR:-$PROJECT_ROOT/data/cache}"
    export RESIN_LOG_DIR="${RESIN_LOG_DIR:-$PROJECT_ROOT/data/log}"

    log_info "监听端口: $RESIN_PORT"
    log_info "监听地址: $RESIN_LISTEN_ADDRESS"
}

# ==================== 代理配置 ====================

setup_proxy() {
    local proxy="http://127.0.0.1:7890"
    export HTTP_PROXY="$proxy"
    export HTTPS_PROXY="$proxy"
    export GOPROXY="https://goproxy.cn,direct"
    log_info "已设置构建代理: $proxy"
}

cleanup_proxy() {
    unset HTTP_PROXY HTTPS_PROXY GOPROXY
    log_info "已清除构建代理"
}

# ==================== 构建 ====================

build_frontend() {
    log_info "构建前端..."

    cd "$PROJECT_ROOT/webui"

    # 安装依赖
    if [ ! -d "node_modules" ] || [ "package.json" -nt "node_modules" ]; then
        log_info "安装前端依赖..."
        npm ci
    fi

    # 构建
    npm run build

    cd "$PROJECT_ROOT"
    log_success "前端构建完成"
}

build_backend() {
    log_info "构建后端 (最小化)..."

    # 获取 git commit 和 build time
    GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
    BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)

    # 最小化构建（不包含可选功能 tags）
    CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags="-s -w \
            -X github.com/Resinat/Resin/internal/buildinfo.Version=dev \
            -X github.com/Resinat/Resin/internal/buildinfo.GitCommit=$GIT_COMMIT \
            -X github.com/Resinat/Resin/internal/buildinfo.BuildTime=$BUILD_TIME" \
        -o "$PROJECT_ROOT/resin" \
        ./cmd/resin

    log_success "后端构建完成: $PROJECT_ROOT/resin"
}

# ==================== 启动 ====================

create_data_dirs() {
    mkdir -p "$RESIN_STATE_DIR" "$RESIN_CACHE_DIR" "$RESIN_LOG_DIR"
}

start_service() {
    echo ""
    echo "=========================================="
    echo "  Resin 启动中..."
    echo "=========================================="
    echo ""
    echo "  管理界面: http://$RESIN_LISTEN_ADDRESS:$RESIN_PORT/ui/platforms"
    echo "  健康检查: http://$RESIN_LISTEN_ADDRESS:$RESIN_PORT/healthz"
    echo "  API 端点: http://$RESIN_LISTEN_ADDRESS:$RESIN_PORT/api"
    echo ""
    echo "  按 Ctrl+C 停止服务"
    echo "=========================================="
    echo ""

    exec "$PROJECT_ROOT/resin"
}

# ==================== 主流程 ====================

main() {
    echo ""
    echo "=========================================="
    echo "  Resin 构建启动脚本"
    echo "=========================================="
    echo ""

    # 1. 检测环境
    detect_os
    detect_arch
    check_go
    check_node

    # 2. 加载环境变量
    load_env

    # 3. 设置构建代理
    setup_proxy

    # 4. 构建
    build_frontend
    build_backend

    # 5. 清除构建代理
    cleanup_proxy

    # 6. 创建数据目录
    create_data_dirs

    # 7. 启动服务
    start_service
}

# 运行主流程
main
