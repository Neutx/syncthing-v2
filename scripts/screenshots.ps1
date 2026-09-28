<#
.SYNOPSIS
    Captures the documentation screenshots from demo mode (synthetic data only).

.DESCRIPTION
    1. Builds stv2 from this checkout into a temporary directory (or uses -Exe).
    2. Starts "stv2 demo --port <Port>". Demo mode serves synthetic devices,
       folders and activity, and a glass backdrop rendered from a synthetic
       wallpaper. It never reads Syncthing, Tailscale or the screen.
    3. Starts Microsoft Edge headless with a fresh, temporary profile and a
       460x640 window:
         msedge --headless=new --window-size=460,640 --remote-debugging-port=0 ...
       For every view it asks the demo server for a single-use login link
       (POST /api/launch with the control token the demo printed), opens
       /?mode=glass&demo=<view> through it, waits for the page to settle and
       saves assets/screenshots/<view>.png through the DevTools protocol.
       Views taller than the window (Pair, Settings) are scrolled by the
       smallest amount that keeps both image edges between rows, so no row or
       section label is cut in half. The Settings view is captured a second
       time, scrolled to its end, as settings-about.png: its About section
       shows the name, version, repository and disclaimer.
       (Edge's --screenshot switch fires before the Pair view has finished
       its discovery round, and --virtual-time-budget never ends because the
       dashboard's state stream is an endless request.)
    4. Renders the tray icon strip to assets/screenshots/tray-states.png with
       "go run ./internal/icon/cmd/genicons -strip" (its regular icon output
       goes to the temporary directory, so assets/icons is not touched).
    5. Closes Edge, stops the demo server and deletes the temporary files.

    Nothing in the output comes from this computer: no user or host names, no
    addresses, no files and no desktop content.

.PARAMETER Port
    Port for the demo server on 127.0.0.1 (default 18999).

.PARAMETER Exe
    An existing console build of stv2 to use instead of building one.

.PARAMETER Edge
    Path of msedge.exe (default: found through the registry or the usual
    install locations).

.PARAMETER Views
    Views to capture (default: status, syncing, pair, settings). Any view of
    "stv2 demo" is accepted.

.PARAMETER Scale
    Device scale factor of the captures (default 2, which gives 920x1280 PNGs
    of the 460x640 dashboard).

.PARAMETER SettleMs
    How long each view may settle after it has loaded (default 2000 ms; the
    demo's pairing discovery takes 600 ms).

.PARAMETER Version
    Version built into stv2 for the captures (default 1.0.0). It appears in
    the About section of settings-about.png; the script fails if it does not.
    Ignored with -Exe, which shows the version that build carries.

.EXAMPLE
    powershell -NoProfile -ExecutionPolicy Bypass -File scripts\screenshots.ps1
#>
[CmdletBinding()]
param(
    [ValidateRange(1024, 65535)][int]$Port = 18999,
    [string]$Exe = '',
    [string]$Edge = '',
    [string[]]$Views = @('status', 'syncing', 'pair', 'settings'),
    [ValidateRange(1, 3)][int]$Scale = 2,
    [ValidateRange(0, 30000)][int]$SettleMs = 2000,
    [string]$Version = '1.0.0'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$Module = 'github.com/Neutx/syncthing-v2'
$Width = 460
$Height = 640
# The views "stv2 demo" understands (internal/ui/demo.go, DemoViews).
$KnownViews = @('status', 'syncing', 'scanning', 'disconnected', 'paused', 'error',
    'down', 'unauthorized', 'notices', 'pair', 'settings')

# "powershell -File" passes -Views a,b as one string.
$Views = @($Views | ForEach-Object { $_ -split ',' } | ForEach-Object { $_.Trim() } | Where-Object { $_ })
foreach ($v in $Views) {
    if ($KnownViews -notcontains $v) {
        throw "Unknown view '$v'. Known views: $($KnownViews -join ', ')."
    }
}
if ($Version -notmatch '^\d+\.\d+\.\d+([-+][0-9A-Za-z.+-]+)?$') {
    throw "Version '$Version' is not a semantic version (x.y.z[-suffix])."
}

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

function Find-Edge {
    foreach ($key in @('HKCU:\SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe',
            'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe',
            'HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe')) {
        $item = Get-ItemProperty -LiteralPath $key -ErrorAction SilentlyContinue
        if ($null -ne $item -and $item.PSObject.Properties['(default)']) {
            $p = $item.'(default)'.Trim('"')
            if ($p -and (Test-Path -LiteralPath $p -PathType Leaf)) { return $p }
        }
    }
    $roots = @(${env:ProgramFiles(x86)}, $env:ProgramFiles, $env:LOCALAPPDATA) | Where-Object { $_ }
    foreach ($r in $roots) {
        $p = Join-Path $r 'Microsoft\Edge\Application\msedge.exe'
        if (Test-Path -LiteralPath $p -PathType Leaf) { return $p }
    }
    throw 'Microsoft Edge (msedge.exe) was not found. Pass its path with -Edge.'
}

function Invoke-Checked {
    param([string]$File, [string[]]$Arguments)
    & $File @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "'$File $($Arguments -join ' ')' failed with exit code $LASTEXITCODE."
    }
}

function Test-PortFree([int]$p) {
    $l = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Loopback, $p)
    try { $l.Start(); return $true } catch { return $false } finally { $l.Stop() }
}

# Reads a file another process still has open for writing.
function Read-SharedText([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    $fs = New-Object System.IO.FileStream($Path, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::ReadWrite)
    try { return (New-Object System.IO.StreamReader($fs)).ReadToEnd() } finally { $fs.Dispose() }
}

# Reads the PNG size from its IHDR chunk.
function Get-PngSize([string]$Path) {
    $b = [IO.File]::ReadAllBytes($Path)
    if ($b.Length -lt 24 -or $b[0] -ne 0x89 -or $b[1] -ne 0x50 -or $b[2] -ne 0x4E -or $b[3] -ne 0x47) {
        throw "$Path is not a PNG file."
    }
    $w = ([int]$b[16] -shl 24) -bor ([int]$b[17] -shl 16) -bor ([int]$b[18] -shl 8) -bor [int]$b[19]
    $h = ([int]$b[20] -shl 24) -bor ([int]$b[21] -shl 16) -bor ([int]$b[22] -shl 8) -bor [int]$b[23]
    return [pscustomobject]@{ Width = $w; Height = $h; Bytes = $b.Length }
}

# ---------- a minimal DevTools protocol client ----------

$script:CdpSocket = $null
$script:CdpId = 0

function Connect-Cdp([string]$Url) {
    $ws = New-Object System.Net.WebSockets.ClientWebSocket
    $ok = $false
    try { $ok = $ws.ConnectAsync([Uri]$Url, [Threading.CancellationToken]::None).Wait(15000) } catch {
        $e = $_.Exception
        while ($e.InnerException) { $e = $e.InnerException }
        throw "Could not connect to the Edge DevTools endpoint: $($e.Message)"
    }
    if (-not $ok) { throw 'Could not connect to the Edge DevTools endpoint within 15 s.' }
    $script:CdpSocket = $ws
}

# Receives one complete WebSocket message as text.
function Receive-CdpMessage([int]$TimeoutMs) {
    $buf = New-Object byte[] 65536
    $ms = New-Object System.IO.MemoryStream
    $cts = New-Object Threading.CancellationTokenSource($TimeoutMs)
    try {
        do {
            $seg = New-Object 'System.ArraySegment[byte]' -ArgumentList @(, $buf)
            $task = $script:CdpSocket.ReceiveAsync($seg, $cts.Token)
            try { $task.Wait() } catch {
                $e = $_.Exception
                while ($e.InnerException) { $e = $e.InnerException }
                throw "No DevTools answer from Edge: $($e.Message)"
            }
            $r = $task.Result
            if ($r.MessageType -eq [System.Net.WebSockets.WebSocketMessageType]::Close) {
                throw 'Edge closed the DevTools connection.'
            }
            $ms.Write($buf, 0, $r.Count)
        } while (-not $r.EndOfMessage)
        return [Text.Encoding]::UTF8.GetString($ms.ToArray())
    } finally {
        $cts.Dispose()
        $ms.Dispose()
    }
}

# Sends a command and returns the raw JSON text of its response. Events that
# arrive in between are skipped.
function Invoke-Cdp([string]$Method, [hashtable]$Params = @{}, [int]$TimeoutMs = 30000) {
    $script:CdpId++
    $id = $script:CdpId
    $json = @{ id = $id; method = $Method; params = $Params } | ConvertTo-Json -Depth 8 -Compress
    Write-Verbose "DevTools -> $Method"
    $bytes = [Text.Encoding]::UTF8.GetBytes($json)
    $seg = New-Object 'System.ArraySegment[byte]' -ArgumentList @(, $bytes)
    $script:CdpSocket.SendAsync($seg, [System.Net.WebSockets.WebSocketMessageType]::Text, $true,
        [Threading.CancellationToken]::None).Wait()
    $deadline = (Get-Date).AddMilliseconds($TimeoutMs)
    while ($true) {
        $left = [int][Math]::Max(1, ($deadline - (Get-Date)).TotalMilliseconds)
        $msg = Receive-CdpMessage $left
        Write-Verbose ("DevTools <- " + $msg.Substring(0, [Math]::Min(160, $msg.Length)))
        if ($msg -match ('^\{"id":' + $id + '[,}]')) {
            if ($msg -match ('^\{"id":' + $id + ',"error":')) { throw "DevTools $Method failed: $msg" }
            return $msg
        }
        if ((Get-Date) -gt $deadline) { throw "DevTools $Method timed out." }
    }
}

# Evaluates a JavaScript expression in the page and returns its string value.
function Get-PageValue([string]$Expression) {
    $msg = Invoke-Cdp 'Runtime.evaluate' @{ expression = "String($Expression)"; returnByValue = $true }
    return ($msg | ConvertFrom-Json).result.result.value
}

# ---------- main ----------

$Root = Split-Path -Parent $PSScriptRoot
$OutDir = Join-Path $Root 'assets\screenshots'
$Tmp = Join-Path ([IO.Path]::GetTempPath()) ('stv2-shots-' + [guid]::NewGuid().ToString('N'))
$ProfileDir = Join-Path $Tmp 'edge-profile'
New-Item -ItemType Directory -Force -Path $Tmp, $OutDir | Out-Null

if (-not $Edge) { $Edge = Find-Edge }
if (-not (Test-Path -LiteralPath $Edge -PathType Leaf)) { throw "Edge was not found at $Edge." }
if (-not (Test-PortFree $Port)) { throw "Port $Port on 127.0.0.1 is in use. Pick another one with -Port." }

# A build of our own carries -Version, and the About capture must show it.
$ExpectVersion = if ($Exe) { '' } else { $Version }
$demo = $null
$browser = $null
$savedEnv = @{ CGO_ENABLED = $env:CGO_ENABLED; GOOS = $env:GOOS; GOARCH = $env:GOARCH }
Push-Location $Root
try {
    $Go = $null
    if ($Exe) {
        $Exe = (Resolve-Path -LiteralPath $Exe).Path
    } else {
        $Go = Find-Go
        $env:CGO_ENABLED = '0'; $env:GOOS = 'windows'; $env:GOARCH = 'amd64'
        $Exe = Join-Path $Tmp 'stv2.exe'
        Write-Host "Building stv2 $Version for the captures"
        # A console build (no -H windowsgui), so the demo's output can be read.
        Invoke-Checked $Go @('build', '-trimpath', '-buildvcs=false',
            '-ldflags', "-X $Module/internal/brand.Version=$Version", '-o', $Exe, './cmd/stv2')
        foreach ($k in @('CGO_ENABLED', 'GOOS', 'GOARCH')) { Remove-Item -Path "Env:$k" -ErrorAction SilentlyContinue }
    }

    # 1. The demo server. Its output carries the control token for /api/launch.
    $outFile = Join-Path $Tmp 'demo.out'
    $errFile = Join-Path $Tmp 'demo.err'
    $demo = Start-Process -FilePath $Exe -ArgumentList @('demo', '--port', "$Port") -PassThru `
        -WindowStyle Hidden -RedirectStandardOutput $outFile -RedirectStandardError $errFile
    $token = $null
    $deadline = (Get-Date).AddSeconds(30)
    while (-not $token) {
        if ($demo.HasExited) {
            throw "stv2 demo exited with code $($demo.ExitCode): $(Read-SharedText $errFile)"
        }
        if ((Get-Date) -gt $deadline) { throw 'stv2 demo did not start within 30 s.' }
        Start-Sleep -Milliseconds 200
        $m = [regex]::Match((Read-SharedText $outFile), 'Authorization: Bearer ([A-Za-z0-9_-]+)')
        if ($m.Success) { $token = $m.Groups[1].Value }
    }
    $base = "http://127.0.0.1:$Port"
    Write-Host "Demo server (synthetic data) on $base"

    # 2. Headless Edge with a throwaway profile and a DevTools endpoint on loopback.
    $edgeArgs = @(
        '--headless=new', '--disable-gpu', '--hide-scrollbars', '--mute-audio',
        '--no-first-run', '--no-default-browser-check', '--disable-extensions', '--disable-sync',
        "--user-data-dir=`"$ProfileDir`"", '--remote-debugging-address=127.0.0.1', '--remote-debugging-port=0',
        "--window-size=$Width,$Height", "--force-device-scale-factor=$Scale",
        '--default-background-color=00000000',
        # With reduced motion the dashboard snaps its springs and skips the
        # entrance animation, so every capture shows the settled layout.
        '--force-prefers-reduced-motion',
        'about:blank')
    $browser = Start-Process -FilePath $Edge -ArgumentList $edgeArgs -PassThru -WindowStyle Hidden
    $portFile = Join-Path $ProfileDir 'DevToolsActivePort'
    $deadline = (Get-Date).AddSeconds(30)
    $devPort = $null
    $browserPath = $null
    while (-not $devPort) {
        if ((Get-Date) -gt $deadline) { throw 'Edge did not open its DevTools endpoint within 30 s.' }
        Start-Sleep -Milliseconds 200
        $lines = (Read-SharedText $portFile) -split "`n"
        if ($lines.Count -ge 2 -and $lines[0].Trim() -match '^\d+$' -and $lines[1].Trim().StartsWith('/devtools/browser/')) {
            $devPort = $lines[0].Trim()
            $browserPath = $lines[1].Trim()
        }
    }
    # A tab of our own (a fresh profile opens first-run pages of its own),
    # opened on the demo's origin: navigating there later from about:blank
    # would swap the renderer process and detach the DevTools session. The
    # page shown is the server's "Forbidden" answer to a login without token.
    # While a fresh profile initialises, Edge may still replace a renderer,
    # so a tab that dies is replaced and the view is tried again.
    function Open-Tab {
        if ($null -ne $script:CdpSocket) { $script:CdpSocket.Dispose(); $script:CdpSocket = $null }
        $tabUrl = "http://127.0.0.1:$devPort/json/new?" + [Uri]::EscapeDataString("$base/login")
        $tab = Invoke-RestMethod -Method Put -Uri $tabUrl -UseBasicParsing
        if (-not $tab.webSocketDebuggerUrl) { throw 'Edge did not open a tab to drive.' }
        Start-Sleep -Milliseconds 500
        Connect-Cdp $tab.webSocketDebuggerUrl
        # New tabs ignore --window-size, so the viewport is set here: the
        # 460x640 dashboard at the chosen scale, transparent outside the
        # squircle, with reduced motion (springs snap, no entrance animation).
        $null = Invoke-Cdp 'Emulation.setDeviceMetricsOverride' @{
            width = $Width; height = $Height; deviceScaleFactor = $Scale; mobile = $false }
        $null = Invoke-Cdp 'Emulation.setDefaultBackgroundColorOverride' @{ color = @{ r = 0; g = 0; b = 0; a = 0 } }
        $null = Invoke-Cdp 'Emulation.setEmulatedMedia' @{
            features = @(@{ name = 'prefers-reduced-motion'; value = 'reduce' }) }
        $null = Get-PageValue 'document.readyState'
    }

    # Scrolls the view's list so that neither edge of the visible area cuts
    # through a row or a section label, and the bottom edge does not leave a
    # section label (or an empty card top) orphaned. "fit" picks the smallest
    # such scroll offset; "end" picks the largest one that shows the whole
    # last section. The chosen offset must keep the bottom edge clean, which
    # is checked here and again right before the capture.
    function Set-ViewScroll([string]$Mode) {
        $js = @'
(() => {
  const mode = '__MODE__';
  const sc = document.querySelector('.view:not([hidden]) .scroll');
  if (!sc) return JSON.stringify({ s: 0, max: 0, bottomClean: true, topClean: true, target: true });
  sc.style.scrollBehavior = 'auto';
  sc.scrollTop = 0;
  const h = sc.clientHeight, max = Math.max(0, sc.scrollHeight - h);
  const top0 = sc.getBoundingClientRect().top;
  const shown = el => el.getClientRects().length > 0;
  const span = el => { const r = el.getBoundingClientRect(); return [r.top - top0, r.bottom - top0]; };
  const all = sel => [...sc.querySelectorAll(sel)].filter(shown);
  const rows = all('.card-list > *, .card-flow, .flow-actions, .section-label').map(span);
  const secs = all('.section').map(el => {
    const first = [...el.querySelectorAll('.card-list > *')].find(shown);
    const whole = span(el);
    return { whole, head: [whole[0], first ? span(first)[1] : whole[1]] };
  });
  const heads = secs.map(x => x.head);
  const cuts = (v, blocks) => blocks.some(([a, b]) => v > a + 1 && v < b - 1);
  const last = secs.length ? secs[secs.length - 1].whole : null;
  let best = null;
  for (let s = 0; s <= max; s++) {
    const t = s, b = s + h;
    const c = {
      s, bottomClean: !cuts(b, rows) && !cuts(b, heads), topClean: !cuts(t, rows),
      target: mode !== 'end' || !last || (last[0] >= t - 1 && last[1] <= b + 1)
    };
    const key = [c.target ? 0 : 1, c.bottomClean ? 0 : 1, c.topClean ? 0 : 1, mode === 'end' ? -s : s];
    const i = best ? key.findIndex((k, j) => k !== best.key[j]) : -1;
    if (!best || (i >= 0 && key[i] < best.key[i])) best = { key, c };
  }
  sc.scrollTop = best.c.s;
  return JSON.stringify(Object.assign({ max, actual: Math.round(sc.scrollTop) }, best.c));
})()
'@
        $r = (Get-PageValue $js.Replace('__MODE__', $Mode)) | ConvertFrom-Json
        if (-not $r.target) { throw "The last section of the view does not fit in the $Height px window." }
        if (-not $r.bottomClean) { throw "No scroll offset keeps the bottom edge of the view clear of a row (mode $Mode)." }
        if (-not $r.topClean) { Write-Warning "The top edge of the capture cuts through a row (mode $Mode, offset $($r.s) px)." }
        Write-Verbose "Scroll ($Mode): offset $($r.s) of $($r.max) px"
        return [int]$r.s
    }

    function Save-Capture([string]$Name, [int]$ScrollTop) {
        # The dashboard may have re-rendered since the scroll was set; the
        # capture must show the checked scroll offset.
        $now = [int](Get-PageValue "(() => { const sc = document.querySelector('.view:not([hidden]) .scroll'); return sc ? Math.round(sc.scrollTop) : 0; })()")
        if ([Math]::Abs($now - $ScrollTop) -gt 1) { throw "The view scrolled to $now px before the '$Name' capture (expected $ScrollTop px)." }
        $msg = Invoke-Cdp 'Page.captureScreenshot' @{ format = 'png'; fromSurface = $true } -TimeoutMs 60000
        $m = [regex]::Match($msg, '"data":"([A-Za-z0-9+/=]+)"')
        if (-not $m.Success) { throw "Edge returned no image for '$Name'." }
        $png = Join-Path $OutDir "$Name.png"
        [IO.File]::WriteAllBytes($png, [Convert]::FromBase64String($m.Groups[1].Value))
        $size = Get-PngSize $png
        if ($size.Width -ne $Width * $Scale -or $size.Height -ne $Height * $Scale) {
            throw "$png is $($size.Width)x$($size.Height), expected $($Width * $Scale)x$($Height * $Scale)."
        }
        Write-Host ("Captured assets/screenshots/{0}.png ({1}x{2}, {3} bytes, scrolled {4} px)" -f $Name, $size.Width, $size.Height, $size.Bytes, $ScrollTop)
    }

    function Save-View([string]$View) {
        $body = @{ next = "/?mode=glass&demo=$View" } | ConvertTo-Json -Compress
        $launch = Invoke-RestMethod -Method Post -Uri "$base/api/launch" -UseBasicParsing `
            -Headers @{ Authorization = "Bearer $token"; 'X-STV2' = '1' } -ContentType 'application/json' -Body $body
        if (-not $launch.ok -or -not $launch.url) { throw "The demo server returned no login link for '$View'." }

        $null = Invoke-Cdp 'Page.navigate' @{ url = [string]$launch.url }
        # The login page redirects to the dashboard; wait until it has loaded.
        $deadline = (Get-Date).AddSeconds(20)
        while ($true) {
            $state = Get-PageValue "location.pathname + '|' + location.search + '|' + document.readyState + '|' + (document.body ? document.body.className : '')"
            if ($state -like "/|*demo=$View*|complete|mode-glass*") { break }
            if ((Get-Date) -gt $deadline) { throw "The '$View' view did not load (last state: $state)." }
            Start-Sleep -Milliseconds 150
        }
        # Time for the first state snapshot and, on the Pair view, the discovery round.
        Start-Sleep -Milliseconds $SettleMs

        $s = Set-ViewScroll 'fit'
        Start-Sleep -Milliseconds 300
        Save-Capture $View $s

        # The About section (name, version, repository, disclaimer) sits
        # below the fold of the Settings view: capture it scrolled to the end.
        if ($View -eq 'settings') {
            $s = Set-ViewScroll 'end'
            Start-Sleep -Milliseconds 300
            $about = Get-PageValue @'
(() => {
  const el = document.getElementById('about-name'), sc = el.closest('.scroll');
  const r = el.getBoundingClientRect(), v = sc.getBoundingClientRect();
  return (r.top >= v.top && r.bottom <= v.bottom ? 'visible|' : 'hidden|') + el.textContent;
})()
'@
            if ($about -notlike "visible|*$ExpectVersion*") {
                throw "The About section is not fully visible or lacks version '$ExpectVersion' (found '$about')."
            }
            Save-Capture 'settings-about' $s
        }
    }

    # 3. One capture per view, each through a fresh single-use login link.
    $attempts = 4
    foreach ($view in $Views) {
        for ($try = 1; ; $try++) {
            try {
                if ($null -eq $script:CdpSocket -or
                    $script:CdpSocket.State -ne [System.Net.WebSockets.WebSocketState]::Open) { Open-Tab }
                Save-View $view
                break
            } catch {
                if ($try -ge $attempts) { throw }
                Write-Host "Retrying '$view' in a new tab ($_)"
                if ($null -ne $script:CdpSocket) { $script:CdpSocket.Dispose(); $script:CdpSocket = $null }
                Start-Sleep -Seconds 1
            }
        }
    }
    # Close Edge through its browser endpoint; it may drop the connection
    # before answering, which is fine.
    try {
        if ($null -ne $script:CdpSocket) { $script:CdpSocket.Dispose(); $script:CdpSocket = $null }
        Connect-Cdp "ws://127.0.0.1:$devPort$browserPath"
        $null = Invoke-Cdp 'Browser.close' @{} -TimeoutMs 10000
    } catch { Write-Verbose "Browser.close: $_" }

    # 4. The tray icon strip (every state, left to right).
    if (-not $Go) { $Go = Find-Go }
    $strip = Join-Path $OutDir 'tray-states.png'
    Invoke-Checked $Go @('run', './internal/icon/cmd/genicons', '-out', (Join-Path $Tmp 'icons'),
        '-strip', $strip, '-strip-size', '64')
    $size = Get-PngSize $strip
    Write-Host ("Rendered assets/screenshots/tray-states.png ({0}x{1})" -f $size.Width, $size.Height)
} finally {
    Pop-Location
    foreach ($k in @($savedEnv.Keys)) {
        if ($null -eq $savedEnv[$k]) { Remove-Item -Path "Env:$k" -ErrorAction SilentlyContinue }
        else { Set-Item -Path "Env:$k" -Value $savedEnv[$k] }
    }
    if ($null -ne $script:CdpSocket) { $script:CdpSocket.Dispose() }
    if ($null -ne $browser) {
        if (-not $browser.WaitForExit(5000)) {
            Stop-Process -Id $browser.Id -Force -ErrorAction SilentlyContinue
        }
        # Edge helper processes that outlive the main one hold the profile.
        Get-CimInstance Win32_Process -Filter "Name='msedge.exe'" -ErrorAction SilentlyContinue |
            Where-Object { $_.CommandLine -and $_.CommandLine.Contains($ProfileDir) } |
            ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
    }
    if ($null -ne $demo -and -not $demo.HasExited) {
        Stop-Process -Id $demo.Id -Force -ErrorAction SilentlyContinue
        $demo.WaitForExit(5000) | Out-Null
    }
    for ($i = 0; $i -lt 10 -and (Test-Path -LiteralPath $Tmp); $i++) {
        Remove-Item -LiteralPath $Tmp -Recurse -Force -ErrorAction SilentlyContinue
        if (Test-Path -LiteralPath $Tmp) { Start-Sleep -Milliseconds 500 }
    }
}
Write-Host 'Done. Review the images before committing them: they must show demo data only.'
