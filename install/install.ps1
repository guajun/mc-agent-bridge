# mc-agent release installer for Windows amd64.
#
# Installs the Go CLI/daemon binary from a versioned GitHub release, verifies
# the published checksum, and can install the matching Toolkit Skill into an
# explicit harness or custom directory. It never installs Python, Go or MCP,
# and it never silently overwrites user content.
#
#   iwr -useb https://raw.githubusercontent.com/guajun/mc-agent-bridge/v0.5.0/install/install.ps1 -OutFile install.ps1
#   ./install.ps1 -Version 0.5.0 -SkillHarness codex
#
# See docs/install.md for the full matrix and examples.
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

function Get-ManifestValue([string]$Path, [string]$Key) {
    if (-not (Test-Path -LiteralPath $Path)) { return "" }
    $line = Get-Content -LiteralPath $Path | Where-Object { $_.StartsWith("$Key=") } | Select-Object -First 1
    if ($null -eq $line) { return "" }
    return $line.Substring($Key.Length + 1)
}

function Write-Manifest([string]$InstalledVersion, [string]$BinaryHash, [string]$Source) {
    $lines = @(
        "product=$Product",
        "version=$InstalledVersion",
        "platform=$Platform",
        "binary_sha256=$BinaryHash",
        "installed_at=$([DateTime]::UtcNow.ToString("yyyy-MM-ddTHH:mm:ssZ"))",
        "source=$Source"
    )
    if ($script:SkillTarget -and (Test-Path -LiteralPath $script:SkillTarget)) {
        $lines += "skill.0.dir=$script:SkillTarget"
        $lines += "skill.0.version=$script:SkillVersion"
        $lines += "skill.0.commit=$script:SkillCommit"
        $lines += "skill.0.tree_sha256=$(Get-TreeHash $script:SkillTarget)"
    }
    Set-Content -LiteralPath (Join-Path $InstallDir "$Product.installed") -Value $lines -Encoding ASCII
}

function Get-SkillRoot([string]$Harness) {
    switch ($Harness) {
        "codex" { Join-Path $(if ($env:CODEX_HOME) { $env:CODEX_HOME } else { Join-Path $env:USERPROFILE ".codex" }) "skills" }
        { $_ -in "claude", "claude-code" } { Join-Path $(if ($env:CLAUDE_CONFIG_DIR) { $env:CLAUDE_CONFIG_DIR } else { Join-Path $env:USERPROFILE ".claude" }) "skills" }
        "universal" { Join-Path $(if ($env:XDG_CONFIG_HOME) { $env:XDG_CONFIG_HOME } else { Join-Path $env:USERPROFILE ".config" }) "agents\skills" }
        "hermes" { Join-Path $(if ($env:HERMES_HOME) { $env:HERMES_HOME } else { Join-Path $env:LOCALAPPDATA "hermes" }) "skills" }
        default { Fail "unknown harness '$Harness'; use -SkillDir DIR for a custom location" }
    }
}

function Get-StateDir {
    if ($env:MC_AGENT_HOME) { return $env:MC_AGENT_HOME }
    return Join-Path $env:AppData "mc-agent"
}

function Fetch-Asset([string]$Name, [string]$Destination) {
    if ($DryRun) { Info "dry-run: would download $Name"; return }
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
    if ($DryRun) { return }
    $expected = ""
    $checksums = Join-Path $Work "checksums.txt"
    if (Test-Path -LiteralPath $checksums) {
        $expected = (Get-Content -LiteralPath $checksums | ForEach-Object {
                $parts = $_ -split "\s+", 2
                if ($parts.Count -eq 2 -and $parts[1].Trim() -eq $Name) { $parts[0] }
            } | Select-Object -First 1)
    }
    if (-not $expected -and $Sha256) { $expected = $Sha256.ToLowerInvariant() }
    if (-not $expected) { Fail "no published checksum found for $Name" }
    $actual = Get-Hash $Path
    if ($actual -ne $expected) { Fail "checksum mismatch for $Name`: expected $expected, got $actual" }
}

# ----------------------------------------------------------------- uninstall

$Work = $null
$SkillTarget = ""
$SkillVersion = ""
$SkillCommit = ""

if ($Uninstall) {
    $manifest = Join-Path $InstallDir "$Product.installed"
    $binary = Join-Path $InstallDir $AssetBinary
    if (-not (Test-Path -LiteralPath $binary)) { Fail "no $Product binary at $binary" }
    if ((Test-Path -LiteralPath $manifest) -and -not $Force) {
        $recorded = Get-ManifestValue $manifest "binary_sha256"
        if ($recorded -and (Get-Hash $binary) -ne $recorded) {
            Fail "$binary differs from the recorded install; pass -Force to remove it anyway"
        }
    }
    if ($DryRun) { Info "dry-run: would remove $binary" }
    else {
        Remove-Item -LiteralPath $binary -Force
        Remove-Item -LiteralPath (Join-Path $InstallDir "$Product.previous.exe") -Force -ErrorAction SilentlyContinue
        Info "removed $binary"
    }
    if ($RemoveSkill -and (Test-Path -LiteralPath $manifest)) {
        $index = 0
        while ($true) {
            $dir = Get-ManifestValue $manifest "skill.$index.dir"
            if (-not $dir) { break }
            $recordedTree = Get-ManifestValue $manifest "skill.$index.tree_sha256"
            if (-not (Test-Path -LiteralPath $dir)) { Info "skill directory is already gone: $dir" }
            elseif ($Force -or (Get-TreeHash $dir) -eq $recordedTree) {
                if ($DryRun) { Info "dry-run: would remove $dir" }
                else { Remove-Item -LiteralPath $dir -Recurse -Force; Info "removed skill $dir" }
            }
            else { Info "keeping modified skill directory (pass -Force to remove): $dir" }
            $index++
        }
    }
    elseif ($RemoveSkill) { Info "no install manifest: not removing any skill directory" }
    if (-not $DryRun) { Remove-Item -LiteralPath $manifest -Force -ErrorAction SilentlyContinue }
    if ($PurgeState) {
        $state = Get-StateDir
        if (Test-Path -LiteralPath $state) {
            if ($DryRun) { Info "dry-run: would remove state $state" }
            else { Remove-Item -LiteralPath $state -Recurse -Force; Info "removed state $state" }
        }
        else { Info "state directory not present: $state" }
    }
    Info "uninstall complete"
    exit 0
}

# ------------------------------------------------------------------ install

if ($SkillHarness -and $SkillDir) { Fail "use either -SkillHarness or -SkillDir, not both" }
if ($NoSkill -and ($SkillHarness -or $SkillDir)) { Fail "use either -NoSkill or a skill target, not both" }

$Work = Join-Path ([System.IO.Path]::GetTempPath()) ("mc-agent-install-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $Work -Force | Out-Null
$ArchiveName = "$Product-$Version-windows-amd64.zip"

try {
    if ($Archive) {
        if (-not (Test-Path -LiteralPath $Archive)) { Fail "archive not found: $Archive" }
        if (-not $DryRun) { Copy-Item -LiteralPath $Archive -Destination (Join-Path $Work $ArchiveName) }
        $sourceDescription = $Archive
    }
    else {
        Fetch-Asset "checksums.txt" (Join-Path $Work "checksums.txt")
        Fetch-Asset $ArchiveName (Join-Path $Work $ArchiveName)
        $sourceDescription = "$BaseUrl/$ArchiveName"
    }
    if (-not $Archive -or $Sha256) { Assert-Checksum (Join-Path $Work $ArchiveName) $ArchiveName }

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    $existingBinary = Join-Path $InstallDir $AssetBinary
    $existingManifest = Join-Path $InstallDir "$Product.installed"
    if ((Test-Path -LiteralPath $existingBinary) -and -not (Test-Path -LiteralPath $existingManifest) -and -not $Force) {
        Fail "$existingBinary exists without an install manifest; pass -Force to replace it"
    }

    if (-not $DryRun) {
        $extract = Join-Path $Work "extract"
        Expand-Archive -LiteralPath (Join-Path $Work $ArchiveName) -DestinationPath $extract -Force
        $extracted = Join-Path $extract $AssetBinary
        if (-not (Test-Path -LiteralPath $extracted)) { Fail "archive does not contain $AssetBinary" }
        if (Test-Path -LiteralPath $existingBinary) {
            Copy-Item -LiteralPath $existingBinary -Destination (Join-Path $InstallDir "$Product.previous.exe") -Force
        }
        Move-Item -LiteralPath $extracted -Destination $existingBinary -Force
    }
    else { Info "dry-run: would install $AssetBinary to $InstallDir" }
    $newHash = if ($DryRun) { "dry-run" } else { Get-Hash $existingBinary }
    Info "installed $Product $Version ($Platform) to $existingBinary"

    # ---------------------------------------------------------------- skill

    if (-not $NoSkill) {
        $skillRoot = ""
        if ($SkillDir) { $skillRoot = $SkillDir }
        elseif ($SkillHarness) { $skillRoot = Get-SkillRoot $SkillHarness }
        if (-not $skillRoot) {
            Info "no skill target given; pass -SkillHarness NAME or -SkillDir DIR to install the Toolkit Skill"
        }
        elseif (-not $DryRun) {
            Fetch-Asset "skill-pin.json" (Join-Path $Work "skill-pin.json")
            $pin = Get-Content -LiteralPath (Join-Path $Work "skill-pin.json") -Raw | ConvertFrom-Json
            $skillAsset = $pin.asset
            $SkillCommit = $pin.commit
            if (-not $skillAsset) { Fail "skill-pin.json does not name a skill asset" }
            Fetch-Asset $skillAsset (Join-Path $Work $skillAsset)
            Assert-Checksum (Join-Path $Work $skillAsset) $skillAsset
            $skillExtract = Join-Path $Work "skill"
            New-Item -ItemType Directory -Path $skillExtract -Force | Out-Null
            $tarExe = Join-Path $env:SystemRoot "System32\tar.exe"
            if (-not (Test-Path -LiteralPath $tarExe)) { $tarExe = "tar" }
            & $tarExe -xzf (Join-Path $Work $skillAsset) -C $skillExtract
            if ($LASTEXITCODE -ne 0) { Fail "cannot extract the skill bundle (tar.exe is required)" }
            if (-not (Test-Path -LiteralPath (Join-Path $skillExtract "minecraft-toolkit\SKILL.md"))) {
                Fail "skill bundle is missing minecraft-toolkit/SKILL.md"
            }
            $script:SkillTarget = Join-Path $skillRoot "minecraft-toolkit"
            $frontmatter = Get-Content -LiteralPath (Join-Path $skillExtract "minecraft-toolkit\SKILL.md") | Select-Object -First 12
            foreach ($line in $frontmatter) {
                if ($line -match '^\s*version:\s*"?([0-9][^"\s]*)"?\s*$') { $script:SkillVersion = $Matches[1]; break }
            }
            if (Test-Path -LiteralPath $script:SkillTarget) {
                if (-not $UpdateSkill) { Fail "$script:SkillTarget already exists; pass -UpdateSkill to replace it (a backup is kept)" }
                $backup = "$script:SkillTarget.backup-" + [DateTime]::UtcNow.ToString("yyyyMMddHHmmss")
                Copy-Item -LiteralPath $script:SkillTarget -Destination $backup -Recurse -Force
                Info "kept a backup at $backup"
                Remove-Item -LiteralPath $script:SkillTarget -Recurse -Force
            }
            New-Item -ItemType Directory -Path $skillRoot -Force | Out-Null
            Copy-Item -LiteralPath (Join-Path $skillExtract "minecraft-toolkit") -Destination $script:SkillTarget -Recurse -Force
            Info "installed skill $script:SkillVersion to $script:SkillTarget"
        }
    }

    if (-not $DryRun) { Write-Manifest $Version $newHash $sourceDescription }

    # ----------------------------------------------------------------- PATH

    if ($AddToPath) {
        if ($DryRun) { Info "dry-run: would add $InstallDir to the user PATH" }
        else {
            $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
            if ($userPath -split ";" -notcontains $InstallDir) {
                $updated = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
                [Environment]::SetEnvironmentVariable("Path", $updated, "User")
                Info "added $InstallDir to the user PATH (open a new shell)"
            }
            else { Info "$InstallDir is already on the user PATH" }
        }
    }

    Info "next: & '$existingBinary' version"
    Info "      & '$existingBinary' daemon start   # then: ... doctor"
}
finally {
    if ($Work -and (Test-Path -LiteralPath $Work)) { Remove-Item -LiteralPath $Work -Recurse -Force -ErrorAction SilentlyContinue }
}
