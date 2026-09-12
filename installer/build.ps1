<#
.SYNOPSIS
  Builds the FuelMind binaries, the update artifact and the MSI.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File installer\build.ps1 -Version 1.1.0

  Produces, in dist\:
    FuelMindCore.exe, fuelmind-launcher.exe, fuelmind-setup.exe,
    fuelmind-devcloud.exe
    fuelmind-core-<version>.zip      update artifact for the cloud
    fuelmind-core-<version>.zip.sha256
    FuelMind-<version>.msi           installer
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][ValidatePattern('^[0-9]+\.[0-9]+\.[0-9]+$')][string]$Version,
    [string]$Dist = "",
    [switch]$SkipMsi
)

$ErrorActionPreference = 'Stop'
$repo = Resolve-Path "$PSScriptRoot\.."
if (-not $Dist) { $Dist = Join-Path (Split-Path $PSScriptRoot -Parent) "dist" }
$Dist = [System.IO.Path]::GetFullPath($Dist)
New-Item -ItemType Directory -Force -Path $Dist | Out-Null

Write-Host "Building FuelMind $Version into $Dist" -ForegroundColor Cyan
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
$ldflags = "-s -w -X main.version=$Version"

$targets = @{
    'FuelMindCore.exe'       = './cmd/fuelmind-core'
    'fuelmind-launcher.exe'  = './cmd/fuelmind-launcher'
    'fuelmind-setup.exe'     = './cmd/fuelmind-setup'
    'fuelmind-devcloud.exe'  = './cmd/fuelmind-devcloud'
}
foreach ($name in $targets.Keys) {
    Push-Location $repo
    try {
        & go build -trimpath -ldflags $ldflags -o (Join-Path $Dist $name) $targets[$name]
        if ($LASTEXITCODE -ne 0) { throw "go build $($targets[$name]) failed" }
    } finally { Pop-Location }
    Write-Host "  built $name"
}

# Update artifact: a zip holding the new core, which the update agent
# verifies by SHA-256 and extracts into versions\<version>\.
$zip = Join-Path $Dist "fuelmind-core-$Version.zip"
Remove-Item $zip -ErrorAction SilentlyContinue
Compress-Archive -Path (Join-Path $Dist 'FuelMindCore.exe') -DestinationPath $zip
$sha = (Get-FileHash $zip -Algorithm SHA256).Hash.ToLower()
Set-Content -Path "$zip.sha256" -Value "$sha  fuelmind-core-$Version.zip" -Encoding ascii
Write-Host "  artifact fuelmind-core-$Version.zip ($sha)"

if ($SkipMsi) { Write-Host 'Skipping MSI.' -ForegroundColor Yellow; exit 0 }

# WiX v3 (candle/light). Install from https://wixtoolset.org/ if missing.
$wix = $env:WIX
if ($wix) { $wixBin = Join-Path $wix 'bin' } else { $wixBin = "$env:LOCALAPPDATA\WiX\tools" }
$candle = Join-Path $wixBin 'candle.exe'
$light = Join-Path $wixBin 'light.exe'
if (-not (Test-Path $candle)) { throw "WiX not found at $wixBin. Install the WiX Toolset v3 or set `$env:WIX." }

$obj = Join-Path $Dist 'product.wixobj'
$msi = Join-Path $Dist "FuelMind-$Version.msi"
& $candle -nologo -arch x64 "-dVersion=$Version" "-dDistDir=$Dist" `
    -ext WixUtilExtension -ext WixFirewallExtension `
    -out $obj "$PSScriptRoot\product.wxs"
if ($LASTEXITCODE -ne 0) { throw 'candle failed' }
& $light -nologo -ext WixUtilExtension -ext WixFirewallExtension -out $msi $obj
if ($LASTEXITCODE -ne 0) { throw 'light failed' }

Write-Host "MSI: $msi" -ForegroundColor Green
Write-Host "Install with: msiexec /i `"$msi`" /qb" -ForegroundColor Green
