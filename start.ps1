# Resin 启动脚本 (Windows PowerShell)
# 按需构建并启动服务,默认直接使用现有 resin.exe
#
# 用法:
#   .\start.ps1                  交互式选择构建方式 (默认直接启动,不重新构建)
#   .\start.ps1 -Build none      跳过构建,直接使用现有 resin.exe
#   .\start.ps1 -Build backend   仅重新构建后端 (Go)
#   .\start.ps1 -Build all       全量重新构建 (前端 + 后端)

param(
    [ValidateSet("auto", "none", "backend", "all")]
    [string]$Build = "auto"
)

$ErrorActionPreference = "Stop"

# 颜色输出函数
function Write-Info { Write-Host "[INFO] $args" -ForegroundColor Blue }
function Write-Ok { Write-Host "[OK] $args" -ForegroundColor Green }
function Write-Warn { Write-Host "[WARN] $args" -ForegroundColor Yellow }
function Write-Err { Write-Host "[ERROR] $args" -ForegroundColor Red }

# 项目根目录
$ProjectRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $ProjectRoot

# ==================== 环境检测 ====================

function Test-Command {
    param([string]$Command)
    $null -ne (Get-Command $Command -ErrorAction SilentlyContinue)
}

function Compare-Version {
    param([string]$Version1, [string]$Version2)
    $v1 = [version]($Version1 -replace '[^0-9.]', '')
    $v2 = [version]($Version2 -replace '[^0-9.]', '')
    return $v1 -ge $v2
}

function Detect-Arch {
    $arch = $env:PROCESSOR_ARCHITECTURE
    switch ($arch) {
        "AMD64" { return "amd64" }
        "ARM64" { return "arm64" }
        default {
            Write-Err "不支持的架构: $arch"
            exit 1
        }
    }
}

function Check-Go {
    Write-Info "检查 Go 环境..."

    if (-not (Test-Command "go")) {
        Write-Err "未检测到 Go 环境"
        Write-Host ""
        Write-Host "请安装 Go 1.25+:"
        Write-Host "  winget install GoLang.Go"
        Write-Host "  或访问: https://go.dev/doc/install"
        Write-Host ""
        exit 1
    }

    $goVersionOutput = go version
    if ($goVersionOutput -match 'go(\d+\.\d+(\.\d+)?)') {
        $goVersion = $Matches[1]
    } else {
        Write-Err "无法解析 Go 版本: $goVersionOutput"
        exit 1
    }

    if (-not (Compare-Version $goVersion "1.25.0")) {
        Write-Err "Go 版本过低: $goVersion (需要 >= 1.25)"
        Write-Host ""
        Write-Host "请升级 Go:"
        Write-Host "  winget upgrade GoLang.Go"
        Write-Host "  或访问: https://go.dev/doc/install"
        Write-Host ""
        exit 1
    }

    Write-Ok "Go 版本: $goVersion"
}

function Check-Node {
    Write-Info "检查 Node.js 环境..."

    if (-not (Test-Command "node")) {
        Write-Err "未检测到 Node.js 环境"
        Write-Host ""
        Write-Host "请安装 Node.js 22+:"
        Write-Host "  winget install OpenJS.NodeJS.LTS"
        Write-Host "  或访问: https://nodejs.org/"
        Write-Host ""
        exit 1
    }

    $nodeVersion = (node -v).TrimStart('v')
    $nodeMajor = [int]($nodeVersion.Split('.')[0])

    if ($nodeMajor -lt 22) {
        Write-Err "Node.js 版本过低: v$nodeVersion (需要 >= 22)"
        Write-Host ""
        Write-Host "请升级 Node.js:"
        Write-Host "  winget upgrade OpenJS.NodeJS.LTS"
        Write-Host "  或访问: https://nodejs.org/"
        Write-Host ""
        exit 1
    }

    Write-Ok "Node.js 版本: v$nodeVersion"

    # 检查 npm
    if (-not (Test-Command "npm")) {
        Write-Err "未检测到 npm"
        exit 1
    }

    $npmVersion = npm -v
    Write-Ok "npm 版本: $npmVersion"
}

# ==================== 环境变量处理 ====================

function Parse-EnvFile {
    param([string]$EnvFile)

    if (-not (Test-Path $EnvFile)) {
        return $false
    }

    $content = Get-Content $EnvFile -ErrorAction Stop
    foreach ($line in $content) {
        # 跳过注释和空行
        if ($line -match '^\s*#' -or $line -match '^\s*$') {
            continue
        }

        # 解析 key=value
        if ($line -match '^([^=]+)=(.*)$') {
            $key = $Matches[1].Trim()
            $value = $Matches[2].Trim()

            # 移除引号
            $value = $value -replace '^["'']|["'']$', ''

            # 设置环境变量
            [Environment]::SetEnvironmentVariable($key, $value, "Process")
        }
    }

    return $true
}

function Load-Env {
    Write-Info "加载环境变量..."

    # 尝试从 .env 文件加载
    if (Parse-EnvFile "$ProjectRoot\.env") {
        Write-Ok "从 .env 文件加载配置"
    } else {
        Write-Warn "未找到 .env 文件"
    }

    # 检查必需的 token
    $adminToken = [Environment]::GetEnvironmentVariable("RESIN_ADMIN_TOKEN", "Process")
    if ([string]::IsNullOrEmpty($adminToken)) {
        Write-Host ""
        Write-Warn "未配置 RESIN_ADMIN_TOKEN"
        $adminToken = Read-Host "请输入管理员 Token"
        if ([string]::IsNullOrEmpty($adminToken)) {
            Write-Err "RESIN_ADMIN_TOKEN 不能为空"
            exit 1
        }
        [Environment]::SetEnvironmentVariable("RESIN_ADMIN_TOKEN", $adminToken, "Process")
    } else {
        $masked = $adminToken.Substring(0, [Math]::Min(8, $adminToken.Length)) + "..."
        Write-Ok "RESIN_ADMIN_TOKEN: $masked"
    }

    $proxyToken = [Environment]::GetEnvironmentVariable("RESIN_PROXY_TOKEN", "Process")
    if ([string]::IsNullOrEmpty($proxyToken)) {
        Write-Host ""
        Write-Warn "未配置 RESIN_PROXY_TOKEN"
        $proxyToken = Read-Host "请输入代理 Token (留空表示无密码)"
        [Environment]::SetEnvironmentVariable("RESIN_PROXY_TOKEN", $proxyToken, "Process")
    } else {
        $masked = $proxyToken.Substring(0, [Math]::Min(8, $proxyToken.Length)) + "..."
        Write-Ok "RESIN_PROXY_TOKEN: $masked"
    }

    # 设置默认值
    [Environment]::SetEnvironmentVariable("RESIN_AUTH_VERSION", "V1", "Process")
    if ([string]::IsNullOrEmpty([Environment]::GetEnvironmentVariable("RESIN_PORT", "Process"))) {
        [Environment]::SetEnvironmentVariable("RESIN_PORT", "2260", "Process")
    }
    if ([string]::IsNullOrEmpty([Environment]::GetEnvironmentVariable("RESIN_LISTEN_ADDRESS", "Process"))) {
        [Environment]::SetEnvironmentVariable("RESIN_LISTEN_ADDRESS", "127.0.0.1", "Process")
    }
    if ([string]::IsNullOrEmpty([Environment]::GetEnvironmentVariable("RESIN_STATE_DIR", "Process"))) {
        [Environment]::SetEnvironmentVariable("RESIN_STATE_DIR", "$ProjectRoot\data\state", "Process")
    }
    if ([string]::IsNullOrEmpty([Environment]::GetEnvironmentVariable("RESIN_CACHE_DIR", "Process"))) {
        [Environment]::SetEnvironmentVariable("RESIN_CACHE_DIR", "$ProjectRoot\data\cache", "Process")
    }
    if ([string]::IsNullOrEmpty([Environment]::GetEnvironmentVariable("RESIN_LOG_DIR", "Process"))) {
        [Environment]::SetEnvironmentVariable("RESIN_LOG_DIR", "$ProjectRoot\data\log", "Process")
    }

    Write-Info "监听端口: $([Environment]::GetEnvironmentVariable('RESIN_PORT', 'Process'))"
    Write-Info "监听地址: $([Environment]::GetEnvironmentVariable('RESIN_LISTEN_ADDRESS', 'Process'))"
}

# ==================== 代理配置 ====================

function Setup-Proxy {
    $proxy = "http://127.0.0.1:7890"
    [Environment]::SetEnvironmentVariable("HTTP_PROXY", $proxy, "Process")
    [Environment]::SetEnvironmentVariable("HTTPS_PROXY", $proxy, "Process")
    [Environment]::SetEnvironmentVariable("GOPROXY", "https://goproxy.cn,direct", "Process")
    Write-Info "已设置构建代理: $proxy"
}

function Cleanup-Proxy {
    [Environment]::SetEnvironmentVariable("HTTP_PROXY", $null, "Process")
    [Environment]::SetEnvironmentVariable("HTTPS_PROXY", $null, "Process")
    [Environment]::SetEnvironmentVariable("GOPROXY", $null, "Process")
    Write-Info "已清除构建代理"
}

# ==================== 构建 ====================

function Build-Frontend {
    Write-Info "构建前端..."

    Set-Location "$ProjectRoot\webui"

    # 安装依赖
    $needInstall = $false
    if (-not (Test-Path "node_modules")) {
        $needInstall = $true
    } elseif ((Get-Item "package.json").LastWriteTime -gt (Get-Item "node_modules").LastWriteTime) {
        $needInstall = $true
    }

    if ($needInstall) {
        Write-Info "安装前端依赖..."
        npm ci
        if ($LASTEXITCODE -ne 0) {
            Write-Err "前端依赖安装失败"
            exit 1
        }
    }

    # 构建
    npm run build
    if ($LASTEXITCODE -ne 0) {
        Write-Err "前端构建失败"
        exit 1
    }

    Set-Location $ProjectRoot
    Write-Ok "前端构建完成"
}

function Build-Backend {
    Write-Info "构建后端 (最小化)..."

    # 获取 git commit 和 build time
    $gitCommit = "unknown"
    try {
        $gitCommit = (git rev-parse --short HEAD 2>$null)
    } catch {}
    $buildTime = (Get-Date -Format "yyyy-MM-ddTHH:mm:ssZ")

    # 最小化构建（不包含可选功能 tags）
    $env:CGO_ENABLED = "0"
    $ldflags = "-s -w -X github.com/Resinat/Resin/internal/buildinfo.Version=dev -X github.com/Resinat/Resin/internal/buildinfo.GitCommit=$gitCommit -X github.com/Resinat/Resin/internal/buildinfo.BuildTime=$buildTime"

    go build `
        -trimpath `
        -ldflags="$ldflags" `
        -o "$ProjectRoot\resin.exe" `
        ./cmd/resin

    if ($LASTEXITCODE -ne 0) {
        Write-Err "后端构建失败"
        exit 1
    }

    Write-Ok "后端构建完成: $ProjectRoot\resin.exe"
}

# ==================== 构建选择 ====================

function Get-BuildMode {
    # 参数显式指定时跳过交互,便于自动化场景
    if ($Build -ne "auto") {
        Write-Info "构建模式 (由参数指定): $Build"
        return $Build
    }

    Write-Host "请选择启动方式:"
    Write-Host "  [1] 直接启动,使用现有 resin.exe (默认,直接回车)"
    Write-Host "  [2] 仅重新构建后端 (Go, 通常只需几秒)"
    Write-Host "  [3] 全量重新构建 (前端 + 后端, 约 30 秒以上)"

    while ($true) {
        $choice = (Read-Host "请输入选择 (1/2/3, 回车=1)").Trim()
        switch ($choice) {
            ""      { return "none" }
            "1"     { return "none" }
            "2"     { return "backend" }
            "3"     { return "all" }
            default { Write-Warn "无效输入: '$choice', 请输入 1、2 或 3" }
        }
    }
}

function Warn-StaleBinary {
    # 直接启动时提示源码可能比二进制新,由用户自行决定是否重建
    $exe = "$ProjectRoot\resin.exe"
    if (-not (Test-Path $exe)) {
        return
    }

    $exeTime = (Get-Item $exe).LastWriteTime
    $scanPaths = @(
        "$ProjectRoot\cmd",
        "$ProjectRoot\internal",
        "$ProjectRoot\webui\src",
        "$ProjectRoot\webui\dist",
        "$ProjectRoot\go.mod"
    )
    $newest = Get-ChildItem -Path $scanPaths -Recurse -File -ErrorAction SilentlyContinue |
        Sort-Object LastWriteTime -Descending |
        Select-Object -First 1

    if ($null -ne $newest -and $newest.LastWriteTime -gt $exeTime) {
        $relPath = $newest.FullName.Substring($ProjectRoot.Length)
        Write-Warn "检测到 $relPath 比 resin.exe 更新, 直接启动将运行旧版本"
    }
}

# ==================== 启动 ====================

function New-DataDirs {
    $stateDir = [Environment]::GetEnvironmentVariable("RESIN_STATE_DIR", "Process")
    $cacheDir = [Environment]::GetEnvironmentVariable("RESIN_CACHE_DIR", "Process")
    $logDir = [Environment]::GetEnvironmentVariable("RESIN_LOG_DIR", "Process")

    New-Item -ItemType Directory -Force -Path $stateDir | Out-Null
    New-Item -ItemType Directory -Force -Path $cacheDir | Out-Null
    New-Item -ItemType Directory -Force -Path $logDir | Out-Null
}

function Start-Service {
    $listenAddr = [Environment]::GetEnvironmentVariable("RESIN_LISTEN_ADDRESS", "Process")
    $port = [Environment]::GetEnvironmentVariable("RESIN_PORT", "Process")

    Write-Host ""
    Write-Host "=========================================="
    Write-Host "  Resin 启动中..."
    Write-Host "=========================================="
    Write-Host ""
    Write-Host "  管理界面: http://${listenAddr}:${port}/ui/platforms"
    Write-Host "  健康检查: http://${listenAddr}:${port}/healthz"
    Write-Host "  API 端点: http://${listenAddr}:${port}/api"
    Write-Host ""
    Write-Host "  按 Ctrl+C 停止服务"
    Write-Host "=========================================="
    Write-Host ""

    & "$ProjectRoot\resin.exe"
}

# ==================== 主流程 ====================

function Main {
    Write-Host ""
    Write-Host "=========================================="
    Write-Host "  Resin 构建启动脚本"
    Write-Host "=========================================="
    Write-Host ""

    # 1. 选择构建方式 (默认直接启动, 不重新构建)
    $buildMode = Get-BuildMode

    # 2. 必要性降级: 缺少可直接运行的产物时自动补建
    if ($buildMode -eq "none") {
        if (-not (Test-Path "$ProjectRoot\resin.exe")) {
            Write-Warn "未找到 resin.exe, 无法直接启动, 将构建后端"
            $buildMode = "backend"
        }
    }
    if ($buildMode -eq "backend") {
        # go:embed 需要 webui/dist 存在
        if (-not (Test-Path "$ProjectRoot\webui\dist\index.html")) {
            Write-Warn "未找到前端构建产物 webui\dist, 将同时构建前端"
            $buildMode = "all"
        }
    }

    # 3. 环境检测 (仅在需要构建时)
    if ($buildMode -ne "none") {
        $arch = Detect-Arch
        Write-Ok "架构: $arch"
        Check-Go
        if ($buildMode -eq "all") {
            Check-Node
        }
    }

    # 4. 加载环境变量
    Load-Env

    # 5. 按需构建
    switch ($buildMode) {
        "all" {
            Setup-Proxy
            Build-Frontend
            Build-Backend
            Cleanup-Proxy
        }
        "backend" {
            Setup-Proxy
            Build-Backend
            Cleanup-Proxy
        }
        "none" {
            Warn-StaleBinary
            Write-Ok "跳过构建, 直接启动现有 resin.exe"
        }
    }

    # 6. 创建数据目录
    New-DataDirs

    # 7. 启动服务
    Start-Service
}

# 运行主流程
Main
