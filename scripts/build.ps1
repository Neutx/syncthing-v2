<#
.SYNOPSIS
    Local Windows build of SyncThing V2. Needs only the Go toolchain.

.DESCRIPTION
    1. Generates the exe resources with go-winres (icon, manifest, version
       info) when packaging/windows/winres.json exists.
    2. Builds dist/stv2.exe (windows/amd64, CGO disabled, GUI subsystem).
    3. Runs go test ./... unless -SkipTests is given.

.PARAMETER Version
    Semantic version baked into the binary (default 0.0.0-dev).

.PARAMETER SkipTests
    Skip go test ./... after the build.

.EXAMPLE
    powershell -NoProfile -ExecutionPolicy Bypass -File scripts\build.ps1 -Version 1.0.0
#>
[CmdletBinding()]
param(
    [string]$Version = '0.0.0-dev',
    [switch]$SkipTests
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$Module = 'github.com/Neutx/syncthing-v2'
$GoWinres = 'github.com/tc-hib/go-winres@v0.3.3'

if ($Version -notmatch '^\d+\.\d+\.\d+([-+][0-9A-Za-z.+-]+)?$') {
    throw "Version '$Version' is not a semantic version (x.y.z[-suffix])."
}
# Windows version resources accept only numeric x.y.z.
$NumericVersion = ($Version -split '[-+]')[0]

function Find-Go {
    $cmd = Get-Command go.exe -ErrorAction SilentlyContinue
    if ($null -ne $cmd) { return $cmd.Source }
    $candidates = @()
    if ($env:LOCALAPPDATA) { $candidates += Join-Path $env:LOCALAPPDATA 'Programs\Go\bin\go.exe' }
    if ($env:ProgramFiles) { $candidates += Join-Path $env:ProgramFiles 'Go\bin\go.exe' }
    foreach ($c in $candidates) {
        if (Test-Path -LiteralPath $c -PathType Leaf) { return $c }
    }
    throw 'Go was not found. Install it from https://go.dev/dl/ and run this script again.'
}

function Invoke-Checked {
    param([string]$Exe, [string[]]$Arguments)
    & $Exe @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "'$Exe $($Arguments -join ' ')' failed with exit code $LASTEXITCODE."
    }
}

$Root = Split-Path -Parent $PSScriptRoot
$Go = Find-Go
$saved = @{ CGO_ENABLED = $env:CGO_ENABLED; GOOS = $env:GOOS; GOARCH = $env:GOARCH }

Push-Location -LiteralPath $Root
try {
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'

    Invoke-Checked $Go @('version')

    $winres = Join-Path $Root 'packaging\windows\winres.json'
    if (Test-Path -LiteralPath $winres -PathType Leaf) {
        Write-Host "Generating exe resources ($GoWinres)"
        Invoke-Checked $Go @('run', $GoWinres, 'make',
            '--in', 'packaging/windows/winres.json',
            '--arch', 'amd64',
            '--out', 'cmd/stv2/rsrc',
            '--product-version', $NumericVersion,
            '--file-version', $NumericVersion)
    } else {
        Write-Host 'packaging/windows/winres.json not found; building without exe resources.'
    }

    $dist = Join-Path $Root 'dist'
    New-Item -ItemType Directory -Force -Path $dist | Out-Null
    $out = Join-Path $dist 'stv2.exe'

    Write-Host "Building $out ($Version)"
    $ldflags = "-s -w -H windowsgui -X $Module/internal/brand.Version=$Version"
    Invoke-Checked $Go @('build', '-trimpath', '-ldflags', $ldflags, '-o', $out, './cmd/stv2')

    if (-not $SkipTests) {
        Write-Host 'Running go test ./...'
        Invoke-Checked $Go @('test', './...')
    }

    $hash = (Get-FileHash -LiteralPath $out -Algorithm SHA256).Hash.ToLowerInvariant()
    Write-Host "Built $out"
    Write-Host "SHA-256 $hash"
} finally {
    Pop-Location
    foreach ($k in $saved.Keys) {
        if ($null -eq $saved[$k]) {
            Remove-Item -LiteralPath "Env:$k" -ErrorAction SilentlyContinue
        } else {
            Set-Item -LiteralPath "Env:$k" -Value $saved[$k]
        }
    }
}
