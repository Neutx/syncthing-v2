# SyncThing V2 installer for Windows (Windows PowerShell 5.1 or later). No admin rights needed.
#
#   [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072; irm https://github.com/Neutx/syncthing-v2/releases/latest/download/install.ps1 | iex
#
# Downloads the release exe and SHA256SUMS.txt, verifies the SHA-256, then runs "stv2.exe install --yes".
# Overrides: $env:STV2_VERSION (release to install, x.y.z) and $env:STV2_BASE_URL (asset base URL).
# The body runs in its own scope so nothing leaks into the calling session under iex.
& {
    Set-StrictMode -Version Latest
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'
    # 1. TLS 1.2 (3072) for GitHub, in case the script is run directly.
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072

    # 2-3. Version (baked in at release time) and download location.
    $ver = '__STV2_VERSION__'
    if ($env:STV2_VERSION) { $ver = $env:STV2_VERSION.Trim().TrimStart('v') }
    if ($ver -notmatch '^\d+\.\d+\.\d+([-+][0-9A-Za-z.+-]+)?$') {
        throw 'This copy of install.ps1 has no release version. Set $env:STV2_VERSION to x.y.z and run it again.'
    }
    $base = "https://github.com/Neutx/syncthing-v2/releases/download/v$ver"
    if ($env:STV2_BASE_URL) { $base = $env:STV2_BASE_URL.Trim().TrimEnd('/') }

    # 4. Architecture: x64 build only; arm64 runs it under emulation.
    if (-not [Environment]::Is64BitOperatingSystem) {
        throw 'SyncThing V2 needs 64-bit Windows 10 or 11.'
    }
    $envKey = 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment'
    $nativeArch = (Get-ItemProperty -LiteralPath $envKey -Name PROCESSOR_ARCHITECTURE).PROCESSOR_ARCHITECTURE
    if ($nativeArch -eq 'ARM64') { Write-Host 'Arm64 Windows detected: running x64 build under emulation.' }

    $name = "SyncThingV2-Setup-$ver-windows-x64.exe"
    $dir = Join-Path ([IO.Path]::GetTempPath()) ('stv2-' + [guid]::NewGuid().ToString())
    try {
        # 5. Download into a fresh temp directory.
        New-Item -ItemType Directory -Path $dir | Out-Null
        $exe = Join-Path $dir $name
        $sums = Join-Path $dir 'SHA256SUMS.txt'
        Write-Host "Downloading SyncThing V2 $ver from $base"
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$name" -OutFile $exe
        Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS.txt" -OutFile $sums

        # 6. Verify the SHA-256 against the matching SHA256SUMS.txt line ("<hash>  <name>" or "<hash> *<name>").
        $want = $null
        foreach ($line in @(Get-Content -LiteralPath $sums)) {
            if ($line -match '^\s*([0-9A-Fa-f]{64}) [ *](.+?)\s*$' -and $Matches[2] -eq $name) {
                $want = $Matches[1].ToLowerInvariant()
            }
        }
        if (-not $want) { throw "SHA256SUMS.txt has no entry for $name. Nothing was installed." }
        $got = (Get-FileHash -LiteralPath $exe -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($got -ne $want) {
            throw "Checksum mismatch for $name (expected $want, got $got). Nothing was installed."
        }
        Write-Host 'Checksum verified.'

        # 7. Install for the current user. The exe is a GUI-subsystem program, so wait on its
        #    process explicitly (only that process, not the tray it launches) and check the exit code.
        Unblock-File -LiteralPath $exe
        $proc = Start-Process -FilePath $exe -ArgumentList @('install', '--yes') -PassThru
        $null = $proc.Handle
        $proc.WaitForExit()
        if ($proc.ExitCode -ne 0) { throw "stv2.exe install --yes failed with exit code $($proc.ExitCode)." }
    } finally {
        # 8. Always remove the temp directory.
        if (Test-Path -LiteralPath $dir) { Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue }
    }

    # 9. Next steps.
    Write-Host ''
    Write-Host "SyncThing V2 $ver is installed."
    Write-Host 'Open SyncThing V2 from the tray. Do the same on your other computer, then click Pair.'
}
