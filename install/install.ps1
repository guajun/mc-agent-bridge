# mc-agent release installer for Windows amd64.
#
# Installs the Go CLI/daemon binary from a versioned GitHub release, verifies
# the published checksum and the embedded version, and can install the matching
# Toolkit Skill into an explicit harness or custom directory. It never installs
# Python, Go or MCP, and it never silently overwrites user content.
#
#   iwr -useb https://raw.githubusercontent.com/guajun/mc-agent-bridge/v0.5.0/install/install.ps1 -OutFile install.ps1
#   ./install.ps1 -Version 0.5.0 -SkillHarness codex
#
# See docs/install.md for the full matrix and examples. The undocumented
# MC_AGENT_INSTALL_FAULT environment variable injects a commit-stage failure
# for the negative acceptance tests (after-binary, after-skill, at-manifest).
[CmdletBinding()]
param(
    [string]$Version = "0.5.0",
    [string]$InstallDir = "",
    [string]$BaseUrl = "",
    [string]$FromDir = "",
    [string]$Archive = "",
    [string]$Sha256 = "",
    [string]$SkillHarness = "",
    [string]$SkillDir = "",
    [switch]$UpdateSkill,
    [switch]$NoSkill,
    [switch]$AddToPath,
    [switch]$DryRun,
    [switch]$Uninstall,
    [switch]$RemoveSkill,
    [switch]$PurgeState,
    [switch]$Force
)

$ErrorActionPreference = "Stop"
$Product = "mc-agent"
$Repo = "guajun/mc-agent-bridge"
$ProductVersion = "0.5.0"
$AssetBinary = "mc-agent.exe"
$ManifestName = "mc-agent.installed"
$PreviousName = "mc-agent.previous.exe"
$ManifestVersion = "2"
$StateFiles = @("daemon.json", "targets.json", "secrets.json", "unknown_writes.json", "daemon.log")
$StateIdentityFiles = @("daemon.json", "targets.json", "secrets.json")

function Fail([string]$Message) { Write-Error "error: $Message"; exit 1 }
function Info([string]$Message) { Write-Host $Message -ForegroundColor DarkGray }

# ------------------------------------------------------------------ platform

$arch = $env:PROCESSOR_ARCHITECTURE
if ($arch -eq "ARM64") {
    Fail "unsupported platform windows/arm64: Windows arm64 is not built or tested by this release line"
}
if (-not [Environment]::Is64BitOperatingSystem) {
    Fail "unsupported platform windows/x86: only Windows amd64 is built and tested"
}
$Platform = "windows/amd64"

if (-not $InstallDir) { $InstallDir = Join-Path $env:LOCALAPPDATA "mc-agent\bin" }
if (-not $BaseUrl) { $BaseUrl = "https://github.com/$Repo/releases/download/v$Version" }
$Fault = "$env:MC_AGENT_INSTALL_FAULT"

# ------------------------------------------------------------------- helpers

function Get-Hash([string]$Path) {
    (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant()
}

function Get-TreeHash([string]$Directory) {
    $lines = Get-ChildItem -LiteralPath $Directory -Recurse -File | Sort-Object FullName | ForEach-Object {
        $relative = $_.FullName.Substring($Directory.Length).TrimStart("\", "/").Replace("\", "/")
        "$(Get-Hash $_.FullName) $relative"
    }
    $joined = ($lines -join "`n") + "`n"
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($joined)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try { ($sha.ComputeHash($bytes) | ForEach-Object { $_.ToString("x2") }) -join "" }
    finally { $sha.Dispose() }
}

function Get-CanonicalPath([string]$Path) {
    $full = [System.IO.Path]::GetFullPath($Path)
    if (Test-Path -LiteralPath $full) { return (Resolve-Path -LiteralPath $full).ProviderPath }
    $parent = Split-Path -Parent $full
    $leaf = Split-Path -Leaf $full
    if (-not $parent -or $parent -eq $full) { return $full }
    return (Join-Path (Get-CanonicalPath $parent) $leaf)
}

function Test-ReparsePoint([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) { return $false }
    $item = Get-Item -LiteralPath $Path -Force
    return [bool]($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint)
}

function Assert-NoReparseAncestors([string]$Path) {
    $parent = Split-Path -Parent $Path
    while ($parent) {
        if (Test-ReparsePoint $parent) {
            Fail "refusing to touch a path through a symlinked/reparse ancestor: $parent"
        }
        $next = Split-Path -Parent $parent
        if (-not $next -or $next -eq $parent) { break }
        $parent = $next
    }
}

function Remove-SafeTree([string]$Path) {
    if (-not $Path) { Fail "refusing to remove an empty path" }
    if (Test-ReparsePoint $Path) { Fail "refusing to remove a reparse point: $Path" }
    if (-not (Test-Path -LiteralPath $Path)) { return }
    $canonical = (Resolve-Path -LiteralPath $Path).ProviderPath.TrimEnd("\", "/")
    $root = [System.IO.Path]::GetPathRoot($canonical).TrimEnd("\", "/")
    if ($canonical -eq $root) { Fail "refusing to remove the filesystem root" }
    $homeCanonical = (Resolve-Path -LiteralPath $env:USERPROFILE).ProviderPath.TrimEnd("\", "/")
    if ($canonical -eq $homeCanonical) { Fail "refusing to remove the home directory" }
    if ($homeCanonical.StartsWith($canonical + "\", [System.StringComparison]::OrdinalIgnoreCase)) {
        Fail "refusing to remove an ancestor of the home directory"
    }
    if ($canonical -eq (Get-CanonicalPath $InstallDir)) { Fail "refusing to remove the install directory" }
    Assert-NoReparseAncestors $canonical
    Remove-Item -LiteralPath $Path -Recurse -Force
}

# Remove only known product-owned state files; keep unrelated files and the
# directory unless it becomes empty.
function Remove-ProductStateFiles([string]$Directory) {
    $found = $false
    foreach ($name in $StateFiles) {
        $file = Join-Path $Directory $name
        if (Test-ReparsePoint $file) { Fail "refusing to remove symlinked state file: $file" }
        if (Test-Path -LiteralPath $file -PathType Container) { Fail "refusing to remove directory-valued state file: $file" }
        if (Test-Path -LiteralPath $file -PathType Leaf) {
            Remove-Item -LiteralPath $file -Force
            Info "removed state file $file"
            $found = $true
        }
    }
    if (-not $found) { Info "no known product state files found in $Directory" }
    $remaining = @(Get-ChildItem -LiteralPath $Directory -Force -ErrorAction SilentlyContinue)
    if ($remaining.Count -eq 0) {
        Remove-Item -LiteralPath $Directory -Force
        Info "removed empty state directory $Directory"
    }
    else { Info "kept state directory with unrelated files: $Directory" }
}

function Get-ManifestValue([string]$Path, [string]$Key) {
    if (-not (Test-Path -LiteralPath $Path)) { return "" }
    $line = Get-Content -LiteralPath $Path -Encoding UTF8 | Where-Object { $_.StartsWith("$Key=") } | Select-Object -First 1
    if ($null -eq $line) { return "" }
    return $line.Substring($Key.Length + 1)
}

function Test-ManifestValid([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) { return $false }
    if ((Get-Item -LiteralPath $Path).Length -eq 0) { return $false }
    if ((Get-ManifestValue $Path "manifest_version") -ne $ManifestVersion) { return $false }
    if ((Get-ManifestValue $Path "product") -ne $Product) { return $false }
    if (-not (Get-ManifestValue $Path "version")) { return $false }
    if (-not (Get-ManifestValue $Path "platform")) { return $false }
    $hash = Get-ManifestValue $Path "binary_sha256"
    if ($hash -notmatch '^[0-9a-f]{64}$') { return $false }
    return $true
}

function Get-ManifestSkillDirs([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) { return @() }
    Get-Content -LiteralPath $Path -Encoding UTF8 | Where-Object { $_ -match '^skill\.\d+\.dir=' } | ForEach-Object {
        $_.Substring($_.IndexOf("=") + 1)
    }
}

function Get-ManifestSkillTree([string]$Path, [string]$Directory) {
    if (-not (Test-Path -LiteralPath $Path)) { return "" }
    $current = ""
    foreach ($line in (Get-Content -LiteralPath $Path -Encoding UTF8)) {
        if ($line -match '^skill\.(\d+)\.dir=(.*)$') {
            $current = if ($Matches[2] -eq $Directory) { "skill.$($Matches[1]).tree_sha256" } else { "" }
            continue
        }
        if ($current -and $line.StartsWith("$current=")) { return $line.Substring($current.Length + 1) }
    }
    return ""
}

function Get-ManifestSkillLines([string]$Path, [string]$SkipTarget) {
    $lines = New-Object System.Collections.Generic.List[string]
    if (-not (Test-Path -LiteralPath $Path)) { return $lines }
    $skip = $false
    foreach ($line in (Get-Content -LiteralPath $Path -Encoding UTF8)) {
        if ($line -match '^skill\.\d+\.dir=') {
            $dir = $line.Substring($line.IndexOf("=") + 1)
            $skip = ($SkipTarget -and $dir -eq $SkipTarget)
        }
        if ($line -match '^skill\.') {
            if (-not $skip) { $lines.Add($line) }
            continue
        }
    }
    return $lines
}

function Get-MaxSkillIndex($Lines) {
    $max = 0
    foreach ($line in $Lines) {
        if ($line -match '^skill\.(\d+)\.dir=') {
            $index = [int]$Matches[1]
            if ($index -gt $max) { $max = $index }
        }
    }
    return $max
}

# Only an absolute path whose leaf is minecraft-toolkit is removable, even
# with -Force.
function Test-SkillEntryRemovable([string]$Path) {
    if (-not [System.IO.Path]::IsPathRooted($Path)) { return $false }
    return ([System.IO.Path]::GetFileName($Path.TrimEnd("\", "/")) -eq "minecraft-toolkit")
}

function Write-Utf8Lines([string]$Path, $Lines) {
    $encoding = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllLines($Path, [string[]]$Lines, $encoding)
}

function Write-Manifest([string]$InstalledVersion, [string]$BinaryHash, [string]$Source) {
    $lines = New-Object System.Collections.Generic.List[string]
    $lines.Add("manifest_version=$ManifestVersion")
    $lines.Add("product=$Product")
    $lines.Add("version=$InstalledVersion")
    $lines.Add("platform=$Platform")
    $lines.Add("binary_sha256=$BinaryHash")
    $lines.Add("installed_at=$([DateTime]::UtcNow.ToString("yyyy-MM-ddTHH:mm:ssZ"))")
    $lines.Add("source=$Source")
    $oldSkillLines = Get-ManifestSkillLines $Manifest $script:SkillTarget
    foreach ($line in $oldSkillLines) { $lines.Add($line) }
    if ($script:SkillTarget) {
        $index = (Get-MaxSkillIndex $oldSkillLines) + 1
        $lines.Add("skill.$index.dir=$script:SkillTarget")
        $lines.Add("skill.$index.version=$script:SkillVersion")
        $lines.Add("skill.$index.commit=$script:SkillCommit")
        $lines.Add("skill.$index.tree_sha256=$(Get-TreeHash $script:SkillTarget)")
    }
    Write-Utf8Lines $Manifest $lines
}

function Get-SkillRoot([string]$Harness) {
    switch ($Harness) {
        "codex" { Join-Path $(if ($env:CODEX_HOME) { $env:CODEX_HOME } else { Join-Path $env:USERPROFILE ".codex" }) "skills" }
        { $_ -in "claude", "claude-code" } { Join-Path $(if ($env:CLAUDE_CONFIG_DIR) { $env:CLAUDE_CONFIG_DIR } else { Join-Path $env:USERPROFILE ".claude" }) "skills" }
        "universal" { Join-Path $(if ($env:XDG_CONFIG_HOME) { $env:XDG_CONFIG_HOME } else { Join-Path $env:USERPROFILE ".config" }) "agents\skills" }
        "hermes" {
            if (-not $env:HERMES_HOME) {
                Fail "the hermes harness has no verified default directory; set HERMES_HOME or pass -SkillDir DIR"
            }
            Join-Path $env:HERMES_HOME "skills"
        }
        default { Fail "unknown harness '$Harness'; use -SkillDir DIR for a custom location" }
    }
}

function Get-StateDir {
    if ($env:MC_AGENT_HOME) { return (Get-CanonicalPath $env:MC_AGENT_HOME) }
    return (Get-CanonicalPath (Join-Path $env:AppData "mc-agent"))
}

function Get-DefaultStateDir {
    return (Get-CanonicalPath (Join-Path $env:AppData "mc-agent"))
}

function Test-ProductStateDir([string]$Directory) {
    if ($Directory -eq (Get-DefaultStateDir)) { return $true }
    foreach ($marker in $StateIdentityFiles) {
        if (Test-Path -LiteralPath (Join-Path $Directory $marker) -PathType Leaf) { return $true }
    }
    return $false
}

function Fetch-Asset([string]$Name, [string]$Destination) {
    if ($FromDir) {
        $source = Join-Path $FromDir $Name
        if (-not (Test-Path -LiteralPath $source)) { Fail "missing staged asset: $source" }
        Copy-Item -LiteralPath $source -Destination $Destination
        return
    }
    Info "downloading $Name"
    Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/$Name" -OutFile $Destination
}

function Assert-Checksum([string]$Path, [string]$Name) {
    $expected = ""
    $checksums = Join-Path $Work "checksums.txt"
    if (Test-Path -LiteralPath $checksums) {
        $expected = (Get-Content -LiteralPath $checksums | ForEach-Object {
                $parts = $_ -split "\s+", 2
                if ($parts.Count -eq 2 -and $parts[1].Trim().TrimStart("*") -eq $Name) { $parts[0] }
            } | Select-Object -First 1)
    }
    if (-not $expected -and $Sha256) { $expected = $Sha256.ToLowerInvariant() }
    if (-not $expected) { Fail "no published checksum found for $Name" }
    $actual = Get-Hash $Path
    if ($actual -ne $expected) { Fail "checksum mismatch for $Name`: expected $expected, got $actual" }
}

function Assert-BinaryVersion([string]$Path, [string]$Want) {
    $output = & $Path version
    if ($LASTEXITCODE -ne 0) { Fail "cannot run the extracted binary: $output" }
    if ($output -notmatch "^mc-agent $([regex]::Escape($Want)) ") {
        Fail "archive does not contain version $Want`: $output"
    }
}

# ------------------------------------------------------------------- plan

$InstallDir = Get-CanonicalPath $InstallDir
$Manifest = Join-Path $InstallDir $ManifestName
$Binary = Join-Path $InstallDir $AssetBinary
$Previous = Join-Path $InstallDir $PreviousName

if ($SkillHarness -and $SkillDir) { Fail "use either -SkillHarness or -SkillDir, not both" }
if ($NoSkill -and ($SkillHarness -or $SkillDir)) { Fail "use either -NoSkill or a skill target, not both" }
if ($Uninstall -and ($Archive -or $FromDir -or $SkillHarness -or $SkillDir)) {
    Fail "-Uninstall cannot be combined with install-only options"
}

$Work = $null
$SkillTarget = ""
$SkillVersion = ""
$SkillCommit = ""

if ($DryRun) {
    Info "dry-run: platform $Platform, version $Version"
    Info "dry-run: would install $Binary (from $BaseUrl/mc-agent-$Version-windows-amd64.zip or staged input)"
    if ($SkillDir) { Info "dry-run: would install the pinned skill into $(Get-CanonicalPath $SkillDir)\minecraft-toolkit" }
    elseif ($SkillHarness) { Info "dry-run: would install the pinned skill into $(Get-SkillRoot $SkillHarness)\minecraft-toolkit" }
    Info "dry-run: would write $Manifest"
    if ($AddToPath) { Info "dry-run: would add $InstallDir to the user PATH" }
    exit 0
}

# ----------------------------------------------------------------- uninstall

if ($Uninstall) {
    if ((-not (Test-Path -LiteralPath $Binary)) -and (-not (Test-Path -LiteralPath $Manifest))) {
        Fail "no $Product installation found at $InstallDir"
    }
    if ((Test-ReparsePoint $Binary) -and -not $Force) { Fail "refusing to remove a symlinked binary: $Binary" }
    if (Test-Path -LiteralPath $Manifest) {
        if (-not (Test-ManifestValid $Manifest) -and -not $Force) {
            Fail "$Manifest is not a valid $Product manifest; pass -Force to remove anyway"
        }
        $recorded = Get-ManifestValue $Manifest "binary_sha256"
        if ((Test-Path -LiteralPath $Binary) -and $recorded -and -not $Force -and (Get-Hash $Binary) -ne $recorded) {
            Fail "$Binary differs from the recorded install; pass -Force to remove it anyway"
        }
    }
    elseif (-not $Force) {
        Fail "$InstallDir has no product manifest; pass -Force to remove $Binary"
    }
    $purgeTarget = ""
    if ($PurgeState) {
        $purgeTarget = Get-StateDir
        if ((Test-Path -LiteralPath $purgeTarget) -and -not (Test-ProductStateDir $purgeTarget)) {
            Fail "refusing -PurgeState: $purgeTarget is not a recognized $Product state directory"
        }
    }
    if ($RemoveSkill -and (Test-Path -LiteralPath $Manifest)) {
        foreach ($dir in @(Get-ManifestSkillDirs $Manifest)) {
            if (-not (Test-SkillEntryRemovable $dir)) {
                Info "refusing to remove a non-skill path from the manifest: $dir"
                continue
            }
            $recordedTree = Get-ManifestSkillTree $Manifest $dir
            if (-not (Test-Path -LiteralPath $dir)) { Info "skill directory is already gone: $dir" }
            elseif ((Get-CanonicalPath $dir) -ne $dir) {
                Info "refusing to remove a skill path that resolves through a link: $dir"
            }
            elseif ($Force -or ($recordedTree -and (Get-TreeHash $dir) -eq $recordedTree)) {
                Remove-SafeTree $dir
                Info "removed skill $dir"
            }
            else { Info "keeping modified skill directory (pass -Force to remove): $dir" }
        }
    }
    elseif ($RemoveSkill) { Info "no install manifest: not removing any skill directory" }
    if (Test-Path -LiteralPath $Binary) { Remove-Item -LiteralPath $Binary -Force }
    if (Test-Path -LiteralPath $Previous) { Remove-Item -LiteralPath $Previous -Force }
    if (Test-Path -LiteralPath $Manifest) { Remove-Item -LiteralPath $Manifest -Force }
    Info "removed the $Product binary from $InstallDir"
    if ($PurgeState -and (Test-Path -LiteralPath $purgeTarget)) {
        Remove-ProductStateFiles $purgeTarget
    }
    Info "uninstall complete"
    exit 0
}

# ------------------------------------------------------------------ stage

$Work = Join-Path ([System.IO.Path]::GetTempPath()) ("mc-agent-install-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $Work -Force | Out-Null
$ArchiveName = "$Product-$Version-windows-amd64.zip"

try {
    if ($Archive) {
        if (-not (Test-Path -LiteralPath $Archive)) { Fail "archive not found: $Archive" }
        Copy-Item -LiteralPath $Archive -Destination (Join-Path $Work $ArchiveName)
        $sourceDescription = $Archive
        if (-not $Sha256) { Info "warning: -Archive without -Sha256; skipping checksum verification" }
        else { Assert-Checksum (Join-Path $Work $ArchiveName) $ArchiveName }
    }
    else {
        Fetch-Asset "checksums.txt" (Join-Path $Work "checksums.txt")
        Fetch-Asset $ArchiveName (Join-Path $Work $ArchiveName)
        $sourceDescription = "$BaseUrl/$ArchiveName"
        Assert-Checksum (Join-Path $Work $ArchiveName) $ArchiveName
    }

    $extract = Join-Path $Work "extract"
    Expand-Archive -LiteralPath (Join-Path $Work $ArchiveName) -DestinationPath $extract -Force
    $extracted = Join-Path $extract $AssetBinary
    if (-not (Test-Path -LiteralPath $extracted)) { Fail "archive does not contain $AssetBinary" }
    Assert-BinaryVersion $extracted $Version

    if ((Test-ReparsePoint $Binary) -and -not $Force) {
        Fail "refusing to replace a symlinked binary: $Binary (pass -Force to replace the link)"
    }
    if (Test-Path -LiteralPath $Binary) {
        if ((Test-Path -LiteralPath $Manifest) -and (Test-ManifestValid $Manifest)) {
            $recorded = Get-ManifestValue $Manifest "binary_sha256"
            if ($recorded -and -not $Force -and (Get-Hash $Binary) -ne $recorded) {
                Fail "$Binary was modified after installation; pass -Force to replace it"
            }
        }
        elseif (-not $Force) {
            Fail "$Binary exists without a valid product manifest; pass -Force to replace it"
        }
    }

    if (-not $NoSkill) {
        $skillRoot = ""
        if ($SkillDir) { $skillRoot = Get-CanonicalPath $SkillDir }
        elseif ($SkillHarness) { $skillRoot = Get-CanonicalPath (Get-SkillRoot $SkillHarness) }
        if (-not $skillRoot) {
            Info "no skill target given; pass -SkillHarness NAME or -SkillDir DIR to install the Toolkit Skill"
        }
        else {
            $script:SkillTarget = Join-Path $skillRoot "minecraft-toolkit"
            if ((Test-Path -LiteralPath $script:SkillTarget) -and -not $UpdateSkill) {
                Fail "$script:SkillTarget already exists; pass -UpdateSkill to replace it (a backup is kept)"
            }
            if (Test-ReparsePoint $script:SkillTarget) { Fail "refusing to replace a symlinked skill directory: $script:SkillTarget" }
            Assert-NoReparseAncestors $script:SkillTarget
            Fetch-Asset "skill-pin.json" (Join-Path $Work "skill-pin.json")
            $pin = Get-Content -LiteralPath (Join-Path $Work "skill-pin.json") -Raw -Encoding UTF8 | ConvertFrom-Json
            if (-not $pin.asset) { Fail "skill-pin.json does not name a skill asset" }
            $script:SkillCommit = $pin.commit
            Fetch-Asset $pin.asset (Join-Path $Work $pin.asset)
            Assert-Checksum (Join-Path $Work $pin.asset) $pin.asset
            $skillExtract = Join-Path $Work "skill"
            New-Item -ItemType Directory -Path $skillExtract -Force | Out-Null
            $tarExe = Join-Path $env:SystemRoot "System32\tar.exe"
            if (-not (Test-Path -LiteralPath $tarExe)) { $tarExe = "tar" }
            & $tarExe -xzf (Join-Path $Work $pin.asset) -C $skillExtract
            if ($LASTEXITCODE -ne 0) { Fail "cannot extract the skill bundle (tar.exe is required)" }
            $skillMd = Join-Path $skillExtract "minecraft-toolkit\SKILL.md"
            if (-not (Test-Path -LiteralPath $skillMd)) { Fail "skill bundle is missing minecraft-toolkit/SKILL.md" }
            foreach ($line in (Get-Content -LiteralPath $skillMd -Encoding UTF8 | Select-Object -First 12)) {
                if ($line -match '^\s*version:\s*"?([0-9][^"\s]*)"?\s*$') { $script:SkillVersion = $Matches[1]; break }
            }
            if (-not $script:SkillVersion) { Fail "cannot read the skill version from the bundle" }
        }
    }

    # ------------------------------------------------------------- commit
    #
    # Snapshot every mutable destination, then apply the changes. Any failure
    # restores all of them, so a failed upgrade never leaves a half-installed
    # product and a fresh failed install leaves no unmanaged binary.

    $snapshot = Join-Path $Work "snapshot"
    New-Item -ItemType Directory -Path $snapshot -Force | Out-Null
    $hadBinary = Test-Path -LiteralPath $Binary
    $hadManifest = Test-Path -LiteralPath $Manifest
    $hadPrevious = Test-Path -LiteralPath $Previous
    $hadSkill = ($script:SkillTarget -and (Test-Path -LiteralPath $script:SkillTarget))
    $createdInstallDir = -not (Test-Path -LiteralPath $InstallDir)
    if ($hadBinary) { Copy-Item -LiteralPath $Binary -Destination (Join-Path $snapshot "binary") -Force }
    if ($hadManifest) { Copy-Item -LiteralPath $Manifest -Destination (Join-Path $snapshot "manifest") -Force }
    if ($hadPrevious) { Copy-Item -LiteralPath $Previous -Destination (Join-Path $snapshot "previous") -Force }
    if ($hadSkill) { Copy-Item -LiteralPath $script:SkillTarget -Destination (Join-Path $snapshot "skill") -Recurse -Force }

    function Remove-IfExists([string]$Path) {
        if ($Path -and (Test-Path -LiteralPath $Path)) { Remove-Item -LiteralPath $Path -Recurse -Force }
    }

    function Invoke-Rollback {
        Remove-IfExists $Binary
        if ($hadBinary) { Copy-Item -LiteralPath (Join-Path $snapshot "binary") -Destination $Binary -Force }
        Remove-IfExists $Manifest
        if ($hadManifest) { Copy-Item -LiteralPath (Join-Path $snapshot "manifest") -Destination $Manifest -Force }
        Remove-IfExists $Previous
        if ($hadPrevious) { Copy-Item -LiteralPath (Join-Path $snapshot "previous") -Destination $Previous -Force }
        if ($script:SkillTarget) {
            Remove-IfExists $script:SkillTarget
            if ($hadSkill) {
                New-Item -ItemType Directory -Path (Split-Path -Parent $script:SkillTarget) -Force | Out-Null
                Copy-Item -LiteralPath (Join-Path $snapshot "skill") -Destination $script:SkillTarget -Recurse -Force
            }
        }
        if ($createdInstallDir) { Remove-Item -LiteralPath $InstallDir -Force -ErrorAction SilentlyContinue }
    }

    function Die-Rollback([string]$Message) {
        Info "rolling back the installation: $Message"
        Invoke-Rollback
        Fail $Message
    }

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    if ($hadBinary) {
        Remove-IfExists $Previous
        Copy-Item -LiteralPath $Binary -Destination $Previous -Force
    }
    Remove-IfExists $Binary
    Copy-Item -LiteralPath $extracted -Destination $Binary -Force
    if ($Fault -eq "after-binary") { Die-Rollback "injected failure: after-binary" }
    $newHash = Get-Hash $Binary
    Info "installed $Product $Version ($Platform) to $Binary"

    if ($script:SkillTarget) {
        New-Item -ItemType Directory -Path (Split-Path -Parent $script:SkillTarget) -Force | Out-Null
        Remove-IfExists $script:SkillTarget
        Copy-Item -LiteralPath (Join-Path $skillExtract "minecraft-toolkit") -Destination $script:SkillTarget -Recurse -Force
        if ($Fault -eq "after-skill") { Die-Rollback "injected failure: after-skill" }
        Info "installed skill $script:SkillVersion to $script:SkillTarget"
    }

    Write-Manifest $Version $newHash $sourceDescription
    if ($Fault -eq "at-manifest") { Die-Rollback "injected failure: at-manifest" }

    # Keep a user-visible backup of the replaced skill (best effort).
    if ($script:SkillTarget -and $hadSkill) {
        $backup = "$script:SkillTarget.backup-" + [DateTime]::UtcNow.ToString("yyyyMMddHHmmss")
        try {
            Copy-Item -LiteralPath (Join-Path $snapshot "skill") -Destination $backup -Recurse -Force
            Info "kept a backup of the previous skill at $backup"
        }
        catch { Info "warning: could not keep a skill backup at $backup" }
    }

    if ($AddToPath) {
        if ($InstallDir -match ";") { Fail "cannot add a directory containing ';' to the user PATH" }
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        if ($userPath -split ";" -notcontains $InstallDir) {
            $updated = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
            [Environment]::SetEnvironmentVariable("Path", $updated, "User")
            Info "added $InstallDir to the user PATH (open a new shell)"
        }
        else { Info "$InstallDir is already on the user PATH" }
    }

    Info "next: & '$Binary' version"
    Info "      & '$Binary' daemon start   # then: ... doctor"
    exit 0
}
finally {
    if ($Work -and (Test-Path -LiteralPath $Work)) { Remove-Item -LiteralPath $Work -Recurse -Force -ErrorAction SilentlyContinue }
}
