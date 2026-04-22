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
if ($Version -eq 'latest') {
    $url = "https://github.com/$Repo/releases/latest/download/$binary"
} else {
    $url = "https://github.com/$Repo/releases/download/$Version/$binary"
}

Write-Host "==> 检测到平台: windows/amd64"
Write-Host "==> 下载 $binary ($Version) 到 $InstallDir"

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$dest = Join-Path $InstallDir 'bbclaw-adapter.exe'

Invoke-WebRequest -Uri $url -OutFile $dest -UseBasicParsing

Write-Host ""
Write-Host "安装完成: $dest"
Write-Host ""
Write-Host "下一步 (参考 docs/skills/bbclaw-adapter-external-install.md):"
Write-Host "  1. 在 $InstallDir 下创建 .env 或通过系统环境变量配置"
Write-Host "     至少: ADAPTER_AUTH_TOKEN / OPENCLAW_WS_URL / ASR_*"
Write-Host "  2. 在 PowerShell 中设置环境变量 (如 `$env:ADAPTER_AUTH_TOKEN='...') 后运行:"
Write-Host "     & '$dest'"
