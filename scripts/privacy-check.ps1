<#
.SYNOPSIS
    Privacy guard: fails when repository content contains an entry of the
    private denylist. The Windows PowerShell twin of scripts/privacy-check.sh.

.DESCRIPTION
    The denylist holds the maintainer's private identifiers (tailnet name,
    tailnet IPs, device and folder IDs, host names, user names, personal
    paths). It is never committed and this script never prints its entries.

    Patterns are newline-separated literal strings, matched case-insensitively,
    taken from $env:PRIVACY_DENYLIST (the CI secret) or else from
    .git/info/privacy-denylist (local, untracked). Blank lines and lines that
    start with '#' are ignored.

    Output names the file and line numbers (or the commit and file) of each
    match, never the matching text. A path that itself contains denylisted
    text is withheld.

    Exit status: 0 clean or skipped, 1 denylisted text found, 2 usage or git error.

.PARAMETER Staged
    Check the staged content (the index) instead of the working tree.

.PARAMETER Untracked
    Also check untracked files that are not ignored.

.PARAMETER History
    Check every commit reachable from any ref: diffs (merge commits and
    binary files included) and messages. Commit
    author and committer names and emails are left out on purpose: they are
    the public identity of the account that pushes, chosen by the maintainer
    (spec D3, e.g. a GitHub noreply address), not repository content.

.PARAMETER Identity
    Check the author and committer identity the next commit would use
    (git var GIT_AUTHOR_IDENT / GIT_COMMITTER_IDENT). .githooks/pre-commit
    runs the shell twin of this check and only warns.

.PARAMETER Require
    Exit 2 when no denylist is available (default: skip with a notice).

.EXAMPLE
    powershell -NoProfile -ExecutionPolicy Bypass -File scripts\privacy-check.ps1 -History
#>
[CmdletBinding()]
param(
    [switch]$Staged,
    [switch]$Untracked,
    [switch]$History,
    [switch]$Identity,
    [switch]$Require
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (@($Staged, $Untracked, $History, $Identity | Where-Object { $_ }).Count -gt 1) {
    [Console]::Error.WriteLine('privacy-check: use only one of -Staged, -Untracked, -History and -Identity.')
    exit 2
}

$top = & git rev-parse --show-toplevel
if ($LASTEXITCODE -ne 0 -or -not $top) {
    [Console]::Error.WriteLine('privacy-check: not inside a git work tree')
    exit 2
}
Set-Location -LiteralPath $top

# Returns the non-empty, non-comment, trimmed lines of $Text.
function Get-Pattern {
    param([string]$Text)
    $out = New-Object System.Collections.Generic.List[string]
    foreach ($line in ($Text -split "`n")) {
        $p = $line.Trim()
        if ($p -ne '' -and -not $p.StartsWith('#')) { $out.Add($p) }
    }
    return , $out.ToArray()
}

if ($env:PRIVACY_DENYLIST) {
    $sourceName = 'the PRIVACY_DENYLIST variable'
    $patterns = Get-Pattern $env:PRIVACY_DENYLIST
} else {
    $localList = & git rev-parse --git-path info/privacy-denylist
    $sourceName = $localList
    if (Test-Path -LiteralPath $localList -PathType Leaf) {
        $patterns = Get-Pattern ([IO.File]::ReadAllText((Resolve-Path -LiteralPath $localList).Path))
    } else {
        $patterns = @()
    }
}
if ($patterns.Count -eq 0) {
    if ($Require) {
        [Console]::Error.WriteLine('privacy-check: no denylist patterns (set PRIVACY_DENYLIST or create .git/info/privacy-denylist)')
        exit 2
    }
    Write-Output 'privacy-check: no denylist (set PRIVACY_DENYLIST or create .git/info/privacy-denylist); skipped.'
    exit 0
}
$count = $patterns.Count

function Test-Denied {
    param([string]$Text)
    foreach ($p in $patterns) {
        if ($Text.IndexOf($p, [StringComparison]::OrdinalIgnoreCase) -ge 0) { return $true }
    }
    return $false
}

$savedEncoding = [Console]::OutputEncoding
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false
$patFile = Join-Path ([IO.Path]::GetTempPath()) ('privacy-check-' + [guid]::NewGuid().ToString() + '.txt')
try {
    # Without a BOM: git would read one as part of the first pattern.
    [IO.File]::WriteAllLines($patFile, [string[]]$patterns, (New-Object System.Text.UTF8Encoding $false))

    if ($Identity) {
        # git var fails when no identity is configured; that is not a privacy problem.
        $ids = @()
        foreach ($name in 'GIT_AUTHOR_IDENT', 'GIT_COMMITTER_IDENT') {
            $savedPreference = $ErrorActionPreference
            $ErrorActionPreference = 'Continue'
            $id = & git var $name 2>$null
            $ErrorActionPreference = $savedPreference
            if ($LASTEXITCODE -eq 0 -and $id) { $ids += [string]$id }
        }
        if ($ids.Count -eq 0) {
            Write-Output 'privacy-check: no commit identity configured; nothing to check.'
            exit 0
        }
        foreach ($id in $ids) {
            if (Test-Denied $id) {
                Write-Output 'privacy-check: the commit author or committer identity (git var GIT_AUTHOR_IDENT / GIT_COMMITTER_IDENT) contains denylisted text (not shown).'
                Write-Output 'Commits made now carry it; set a repo-local identity, e.g. a GitHub noreply email, if it should stay private.'
                exit 1
            }
        }
        Write-Output "privacy-check: commit identity is clean ($count patterns from $sourceName)."
        exit 0
    }

    if ($History) {
        $first = & git rev-list --all -n 1
        if ($LASTEXITCODE -ne 0) { exit 2 }
        if (-not $first) {
            Write-Output 'privacy-check: no commits yet; history is clean.'
            exit 0
        }
        # "`u0001" marks the start of each commit's message. Author and
        # committer are left out on purpose (see -Identity).
        #   -m      diff merge commits against each parent, so text introduced
        #           while resolving a merge is scanned (plain -p shows no merge diff)
        #   --text  show binary blobs as text instead of "Binary files differ"
        #   --root  diff the root commit even when log.showRoot is false
        #   prefixes fixed so user diff settings cannot break the path parsing
        $log = & git log --all -p -m --text --root --no-color --no-ext-diff --no-textconv '--src-prefix=a/' '--dst-prefix=b/' '--format=%x01commit %H%n%B'
        if ($LASTEXITCODE -ne 0) { exit 2 }
        $commit = ''
        $where = '(before the first commit header)'
        $hits = New-Object System.Collections.Generic.List[string]
        foreach ($line in @($log)) {
            if ($line.StartsWith([string][char]1)) {
                $commit = $line.Substring(8, [Math]::Min(12, $line.Length - 8))
                $where = 'commit message'
            } elseif ($line.StartsWith('diff --git ')) {
                $k = $line.IndexOf(' b/')
                if ($k -lt 0) { $k = $line.IndexOf(' "b/') }
                $path = $line.Substring($k + 1).Trim('"')
                if ($path.StartsWith('b/')) { $path = $path.Substring(2) }
                if (Test-Denied $path) { $where = '(a path that contains denylisted text)' } else { $where = $path }
            }
            if (Test-Denied $line) {
                $key = "$commit $where"
                if (-not $hits.Contains($key)) { $hits.Add($key) }
            }
        }
        if ($hits.Count -eq 0) {
            Write-Output "privacy-check: history is clean ($count patterns from $sourceName)."
            exit 0
        }
        Write-Output 'privacy-check: denylisted text found in history (the patterns are not shown):'
        foreach ($h in $hits) { Write-Output "  $h" }
        Write-Output 'Rewriting history is needed to remove it; do not push these commits.'
        exit 1
    }

    $scope = @()
    $mode = 'tree'
    if ($Staged) { $scope = @('--cached'); $mode = 'staged' }
    if ($Untracked) { $scope = @('--untracked'); $mode = 'untracked' }

    # 1. File contents, including binary files (-a).
    $raw = & git grep @scope -a -l -z -F -i -f $patFile
    $rc = $LASTEXITCODE
    if ($rc -gt 1) { exit 2 }
    $files = @(([string]::Join("`n", @($raw))) -split "`0" | ForEach-Object { $_.Trim("`n") } | Where-Object { $_ -ne '' })

    # 2. Path names.
    if ($Untracked) {
        $rawPaths = & git ls-files -z --cached --others --exclude-standard
    } else {
        $rawPaths = & git ls-files -z --cached
    }
    if ($LASTEXITCODE -ne 0) { exit 2 }
    $paths = @(([string]::Join("`n", @($rawPaths))) -split "`0" | ForEach-Object { $_.Trim("`n") } | Where-Object { $_ -ne '' })

    $found = $false
    if ($rc -eq 0 -and $files.Count -gt 0) {
        $found = $true
        Write-Output 'privacy-check: denylisted text found (the patterns are not shown):'
        foreach ($f in $files) {
            $entry = [Array]::IndexOf($paths, $f) + 1
            if (Test-Denied $f) { $shown = "(a path that contains denylisted text; git ls-files entry $entry)" } else { $shown = $f }
            $lineNumbers = @(& git grep @scope -I -h -n -F -i -f $patFile -- ":(literal)$f" | ForEach-Object { $_.Split(':')[0] })
            if ($lineNumbers.Count -gt 0) {
                Write-Output ("  {0}: line {1}" -f $shown, ($lineNumbers -join ','))
            } else {
                Write-Output "  ${shown}: binary file"
            }
        }
    }
    $badPaths = @()
    for ($i = 0; $i -lt $paths.Count; $i++) {
        if (Test-Denied $paths[$i]) { $badPaths += ($i + 1) }
    }
    if ($badPaths.Count -gt 0) {
        $found = $true
        Write-Output 'privacy-check: file names that contain denylisted text (entries of git ls-files):'
        foreach ($n in $badPaths) { Write-Output "  entry $n" }
    }
    if ($found) {
        Write-Output "Remove the personal data before committing. The denylist has $count patterns from $sourceName."
        exit 1
    }
    Write-Output "privacy-check: clean ($mode, $count patterns from $sourceName)."
    exit 0
} finally {
    [Console]::OutputEncoding = $savedEncoding
    Remove-Item -LiteralPath $patFile -Force -ErrorAction SilentlyContinue
}
