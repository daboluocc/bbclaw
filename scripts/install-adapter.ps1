# install-adapter.ps1 — 一键下载最新 bbclaw-adapter (Windows)
#
# 用法:
#   iwr -useb https://raw.githubusercontent.com/daboluocc/bbclaw/main/scripts/install-adapter.ps1 | iex
#
# 环境变量:
#   BBCLAW_INSTALL_DIR   安装目录 (默认 $HOME\bbclaw-adapter)
#   BBCLAW_VERSION       指定版本 tag (默认 latest)

$ErrorActionPreference = 'Stop'

$Repo = 'daboluocc/bbclaw'
$InstallDir = if ($env:BBCLAW_INSTALL_DIR) { $env:BBCLAW_INSTALL_DIR } else { Join-Path $HOME 'bbclaw-adapter' }
$Version = if ($env:BBCLAW_VERSION) { $env:BBCLAW_VERSION } else { 'latest' }

$arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLower()
if ($arch -notin @('x64', 'amd64')) {
    Write-Error "目前仅发布 Windows x64 二进制，当前架构: $arch"
}

$binary = 'bbclaw-adapter-windows-amd64.exe'

Write-Host "==> 检测到平台: windows/amd64"

# 最新的 firmware-only release 不带 adapter 二进制，不能直接用 /releases/latest/download/。
# 从 releases.atom（公开 RSS，不受 API 限流影响）拿 tag 列表，再对每个 tag HEAD
# 一次资产 URL，第一个返回 200/302 的就是最新可用版本。
if ($Version -eq 'latest') {
    Write-Host "==> 查询最新带 adapter 二进制的 release"
    $atom = Invoke-WebRequest -Uri "https://github.com/$Repo/releases.atom" -UseBasicParsing
    $tagRegex = [regex]"/$Repo/releases/tag/([^`"<]+)"
    $tags = $tagRegex.Matches($atom.Content) | ForEach-Object { $_.Groups[1].Value }

    $url = $null
    foreach ($tag in $tags) {
        $candidate = "https://github.com/$Repo/releases/download/$tag/$binary"
        try {
            $resp = Invoke-WebRequest -Uri $candidate -Method Head -UseBasicParsing -MaximumRedirection 0 -ErrorAction Stop
            if ($resp.StatusCode -eq 200 -or $resp.StatusCode -eq 302) {
                $url = $candidate; Write-Host "==> 命中 $tag"; break
            }
        } catch {
            # 302 在严格模式下会抛异常，但我们能从异常里读到状态码
            $code = $_.Exception.Response.StatusCode.value__
            if ($code -eq 302 -or $code -eq 200) {
                $url = $candidate; Write-Host "==> 命中 $tag"; break
            }
        }
    }

    if (-not $url) {
        Write-Error "在最近的 release 中找不到 $binary。请到 https://github.com/$Repo/releases 手动下载，或用 `$env:BBCLAW_VERSION='vX.Y.Z' 指定版本"
    }
} else {
    $url = "https://github.com/$Repo/releases/download/$Version/$binary"
}

Write-Host "==> 下载 $url 到 $InstallDir"

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$dest = Join-Path $InstallDir 'bbclaw-adapter.exe'

Invoke-WebRequest -Uri $url -OutFile $dest -UseBasicParsing

# 把 $InstallDir 加进用户级 PATH（持久化），使任意位置都能 `bbclaw-adapter ...`。
# 设备配置功能（butler 会让 AI 直接敲 `bbclaw-adapter device ...`）依赖它在 PATH 里，
# 否则会因找不到命令而失败。
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$onPath = $false
if ($userPath) {
    $onPath = ($userPath -split ';') -contains $InstallDir
}
if (-not $onPath) {
    $newPath = if ([string]::IsNullOrEmpty($userPath)) { $InstallDir } else { "$userPath;$InstallDir" }
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
    Write-Host "==> 已把 $InstallDir 加入用户 PATH（新开的终端生效）"
}
# 让当前会话立即可用
if (($env:Path -split ';') -notcontains $InstallDir) {
    $env:Path = "$env:Path;$InstallDir"
}

Write-Host ""
Write-Host "安装完成: $dest"
Write-Host "已加入 PATH: $InstallDir（新终端里可直接运行 bbclaw-adapter）"
Write-Host ""
Write-Host "下一步 (参考 docs/skills/bbclaw-adapter-external-install.md):"
Write-Host "  1. 在 $InstallDir 下创建 .env 或通过系统环境变量配置"
Write-Host "     至少: ADAPTER_AUTH_TOKEN / OPENCLAW_WS_URL / ASR_*"
Write-Host "  2. 在 PowerShell 中设置环境变量 (如 `$env:ADAPTER_AUTH_TOKEN='...') 后运行:"
Write-Host "     bbclaw-adapter"
Write-Host ""
Write-Host "验证 CLI 可被找到:"
Write-Host "  Get-Command bbclaw-adapter"
