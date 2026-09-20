<#
.SYNOPSIS
    打 CLIProxyAPI 插件商店要求的 release 资产。

.DESCRIPTION
    产出两样东西，正好是 registry 安装器校验的格式：
      <id>_<version>_<goos>_<goarch>.zip
      checksums.txt              （sha256sum 格式）

    两个硬性约定（错了安装器会拒）：
      1. zip **根目录**必须直接放动态库，不能套子目录，且只能有一个动态库；
      2. checksums.txt 用 "<sha256>  <文件名>" 两空格分隔的标准格式。

.PARAMETER Version
    版本号，不带前导 v。GitHub release 的 tag 要写成 v<Version>。

.PARAMETER GOOS / GOARCH
    目标平台，默认取本机。交叉编译 c-shared 需要对应平台的 C 工具链，
    所以实际发布通常交给 CI（见 .github/workflows/release.yml）。

.EXAMPLE
    pwsh -File package.ps1 -Version 0.1.0

.EXAMPLE
    # 只打包不重新编译
    pwsh -File package.ps1 -Version 0.1.0 -SkipBuild
#>
[CmdletBinding()]
param(
    [string]$Version = "0.1.0",
    [string]$GOOS,
    [string]$GOARCH,
    [switch]$SkipBuild
)

$ErrorActionPreference = "Stop"
Set-Location -LiteralPath $PSScriptRoot

$PluginId = "mimo"
$Dist = Join-Path $PSScriptRoot "dist"

if (-not $GOOS)   { $GOOS   = (& go env GOOS).Trim() }
if (-not $GOARCH) { $GOARCH = (& go env GOARCH).Trim() }

$Ext = switch ($GOOS) {
    "windows" { "dll" }
    "darwin"  { "dylib" }
    default   { "so" }
}

$LibName = "$PluginId.$Ext"
$ZipName = "${PluginId}_${Version}_${GOOS}_${GOARCH}.zip"
$Lib     = Join-Path $Dist $LibName
$Zip     = Join-Path $Dist $ZipName

if ($Version.StartsWith("v")) {
    throw "Version 不要带前导 v（tag 才需要），当前: $Version"
}

Write-Host "==> 目标 $GOOS/$GOARCH  版本 $Version" -ForegroundColor Cyan

if (-not $SkipBuild) {
    New-Item -ItemType Directory -Force -Path $Dist | Out-Null
    $env:CGO_ENABLED = "1"   # -buildmode=c-shared 必须
    Write-Host "==> go build -buildmode=c-shared"
    & go build -buildmode=c-shared -trimpath -ldflags "-s -w" -o $Lib .
    if ($LASTEXITCODE -ne 0) { throw "编译失败" }
}

if (-not (Test-Path $Lib)) { throw "找不到动态库: $Lib（先去掉 -SkipBuild 编译一次）" }

# 暂存目录，保证 zip 根目录只有那一个动态库
$Stage = Join-Path $Dist "stage"
if (Test-Path $Stage) { Remove-Item $Stage -Recurse -Force }
New-Item -ItemType Directory -Force -Path $Stage | Out-Null
Copy-Item $Lib (Join-Path $Stage $LibName) -Force

if (Test-Path $Zip) { Remove-Item $Zip -Force }
Compress-Archive -Path (Join-Path $Stage $LibName) -DestinationPath $Zip -CompressionLevel Optimal

# sha256sum 格式：<hash>  <filename>（两个空格）
$hash = (Get-FileHash -Path $Zip -Algorithm SHA256).Hash.ToLower()
"$hash  $ZipName" | Set-Content -Path (Join-Path $Dist "checksums.txt") -Encoding ascii -NoNewline
Add-Content -Path (Join-Path $Dist "checksums.txt") -Value "" -Encoding ascii

Remove-Item $Stage -Recurse -Force

Write-Host ""
Write-Host "==> 产物" -ForegroundColor Green
Get-ChildItem $Dist -File | ForEach-Object {
    "{0,-48} {1,8:N1} KB" -f $_.Name, ($_.Length / 1KB)
}

# 校验 zip 根目录布局（这是安装器最容易拒的地方）
Add-Type -AssemblyName System.IO.Compression.FileSystem
$zipHandle = [System.IO.Compression.ZipFile]::OpenRead($Zip)
try {
    # 必须用 @() 强制成数组：PowerShell 里单个字符串的 [0] 取的是**首字符**，
    # 不加这层 $entries[0] 会得到 "m" 而不是 "mimo.dll"。
    $entries = @($zipHandle.Entries | ForEach-Object { $_.FullName })
} finally {
    $zipHandle.Dispose()
}
Write-Host ""
Write-Host "==> zip 内容（应只有 $LibName，且无目录）"
$entries | ForEach-Object { "    $_" }
if ($entries.Count -ne 1 -or $entries[0] -ne $LibName) {
    throw "zip 布局不对：根目录必须只有 $LibName，实际: $($entries -join ', ')"
}
Write-Host "布局校验通过 ✅" -ForegroundColor Green
Write-Host ""
Write-Host "checksums.txt:" -ForegroundColor Cyan
Get-Content (Join-Path $Dist "checksums.txt") | ForEach-Object { "    $_" }
Write-Host ""
Write-Host "发布时: tag 用 v$Version，把 $ZipName 和 checksums.txt 一起传上去" -ForegroundColor Cyan
