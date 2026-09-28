# Release-installed acceptance for Windows amd64. Runs the real install script
# against staged release assets and exercises install/version/doctor/upgrade/
# uninstall/skill behavior, including negative transaction, ownership and
# filesystem-safety cases.
#
#   pwsh release/acceptance/install_acceptance.ps1 `
#       -Assets dist -PreviousAssets dist-previous -Version 0.5.0 -Work $env:TEMP\mc-agent-acceptance
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Assets,
    [string]$PreviousAssets = "",
    [Parameter(Mandatory = $true)][string]$Version,
    [Parameter(Mandatory = $true)][string]$Work,
    [string]$Installer = "install/install.ps1"
)

$ErrorActionPreference = "Stop"
$Assets = (Resolve-Path -LiteralPath $Assets).Path
$Installer = (Resolve-Path -LiteralPath $Installer).Path
$PreviousVersion = "0.4.9-ci"
$script:Failures = 0

# The work directory is deleted recursively; only accept an empty directory or
# one carrying our own marker.
if (Test-Path -LiteralPath $Work) {
    $entries = @(Get-ChildItem -LiteralPath $Work -Force -ErrorAction SilentlyContinue)
    if (-not (Test-Path -LiteralPath (Join-Path $Work ".mc-agent-acceptance")) -and $entries.Count -gt 0) {
        throw "error: -Work $Work is not empty and has no .mc-agent-acceptance marker"
    }
}
if (Test-Path -LiteralPath $Work) { Remove-Item -LiteralPath $Work -Recurse -Force }
New-Item -ItemType Directory -Path (Join-Path $Work "bin"), (Join-Path $Work "home"), (Join-Path $Work "skills") -Force | Out-Null
Set-Content -LiteralPath (Join-Path $Work ".mc-agent-acceptance") -Value "acceptance" -Encoding ASCII
$BinDir = Join-Path $Work "bin"
$Binary = Join-Path $BinDir "mc-agent.exe"
$Manifest = Join-Path $BinDir "mc-agent.installed"
$env:MC_AGENT_HOME = Join-Path $Work "home"

function Check([string]$Name, [bool]$Ok) {
    if ($Ok) { Write-Host "PASS  $Name" }
    else { Write-Host "FAIL  $Name" -ForegroundColor Red; $script:Failures++ }
}
function Skip([string]$Name) { Write-Host "SKIP  $Name" }
function HashFile([string]$Path) { (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant() }

function Get-TreeHash([string]$Directory) {
    $lines = Get-ChildItem -LiteralPath $Directory -Recurse -File | Sort-Object FullName | ForEach-Object {
        $relative = $_.FullName.Substring($Directory.Length).TrimStart("\", "/").Replace("\", "/")
        "$(HashFile $_.FullName) $relative"
    }
    $joined = ($lines -join "`n") + "`n"
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($joined)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try { ($sha.ComputeHash($bytes) | ForEach-Object { $_.ToString("x2") }) -join "" }
    finally { $sha.Dispose() }
}

function Invoke-Installer([string[]]$Arguments, [string]$Fault = "") {
    if ($Fault) { $env:MC_AGENT_INSTALL_FAULT = $Fault }
    else { Remove-Item Env:MC_AGENT_INSTALL_FAULT -ErrorAction SilentlyContinue }
    & pwsh -NoProfile -File $Installer @Arguments 2>&1 | Out-Null
    $code = $LASTEXITCODE
    Remove-Item Env:MC_AGENT_INSTALL_FAULT -ErrorAction SilentlyContinue
    return $code
}

function New-Checksums([string]$Directory, [string[]]$Patterns) {
    $lines = New-Object System.Collections.Generic.List[string]
    foreach ($pattern in $Patterns) {
        foreach ($file in @(Get-ChildItem -LiteralPath $Directory -Filter $pattern -File -ErrorAction SilentlyContinue)) {
            $lines.Add("$(HashFile $file.FullName)  $($file.Name)")
        }
    }
    Set-Content -LiteralPath (Join-Path $Directory "checksums.txt") -Value $lines -Encoding ASCII
}

function Wait-BinaryIdle {
    for ($i = 0; $i -lt 10; $i++) {
        $running = Get-CimInstance Win32_Process -Filter "Name='mc-agent.exe'" -ErrorAction SilentlyContinue |
            Where-Object { $_.ExecutablePath -eq $Binary }
        if (-not $running) { return }
        Start-Sleep -Seconds 1
    }
}

# The previous-version fixtures are stage-only; give them the checksums and
# skill files the installer expects.
if ($PreviousAssets) {
    $PreviousAssets = (Resolve-Path -LiteralPath $PreviousAssets).Path
    $stage = Join-Path $Work "previous-assets"
    New-Item -ItemType Directory -Path $stage -Force | Out-Null
    Copy-Item -Path (Join-Path $PreviousAssets '*') -Destination $stage -Force
    Copy-Item -LiteralPath (Join-Path $Assets "skill-pin.json") -Destination $stage -Force
    $pin = Get-Content -LiteralPath (Join-Path $Assets "skill-pin.json") -Raw -Encoding UTF8 | ConvertFrom-Json
    Copy-Item -LiteralPath (Join-Path $Assets $pin.asset) -Destination $stage -Force
    New-Checksums $stage @("*.zip", "*.tar.gz", "*.tgz")
    $PreviousAssets = $stage
}

# ---------------------------------------------------------- baseline install

if ($PreviousAssets) {
    $null = Invoke-Installer @("-Version", $PreviousVersion, "-FromDir", $PreviousAssets, "-InstallDir", $BinDir, "-SkillDir", (Join-Path $Work "skills"))
    $baselineVersion = $PreviousVersion
}
else {
    $null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-SkillDir", (Join-Path $Work "skills"))
    $baselineVersion = $Version
}
Check "installed binary reports version $baselineVersion" ((& $Binary version) -match "mc-agent $([regex]::Escape($baselineVersion)) ")
$baseSkill = Join-Path $Work "skills\minecraft-toolkit"
Check "baseline skill installed" (Test-Path -LiteralPath (Join-Path $baseSkill "SKILL.md"))
$baseBinaryHash = HashFile $Binary
$baseManifestHash = HashFile $Manifest
$baseSkillHash = Get-TreeHash $baseSkill

$null = & $Binary daemon start --fake
Check "capabilities lists state through the daemon" ((& $Binary capabilities | Out-String) -match '"state"')
Check "state answers through the fake mod" ((& $Binary state | Out-String) -match '"stub":true')
$doctor = (& $Binary doctor | Out-String)
Check "doctor reports the installed version" ($doctor -match ('"check":"version","detail":"mc-agent ' + [regex]::Escape($baselineVersion)))
Check "doctor sees the running daemon" ($doctor -match '"check":"daemon","detail":"running"')
$null = & $Binary daemon stop
Wait-BinaryIdle

# ------------------------------------------------- injected commit failures

if ($PreviousAssets) {
    foreach ($fault in @("after-binary", "after-skill", "at-manifest")) {
        $null = Invoke-Installer -Arguments @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-SkillDir", (Join-Path $Work "skills"), "-UpdateSkill") -Fault $fault
        Check "injected $fault failure aborts the install" ($LASTEXITCODE -ne 0)
        Check "$fault`: original binary preserved" ((HashFile $Binary) -eq $baseBinaryHash)
        Check "$fault`: original manifest preserved" ((HashFile $Manifest) -eq $baseManifestHash)
        Check "$fault`: original skill tree preserved" ((Get-TreeHash $baseSkill) -eq $baseSkillHash)
    }
    $null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", (Join-Path $Work "fresh\bin"), "-NoSkill") "after-binary"
    Check "fresh injected failure aborts the install" ($LASTEXITCODE -ne 0)
    Check "fresh failed install left no binary or manifest" (
        (-not (Test-Path -LiteralPath (Join-Path $Work "fresh\bin\mc-agent.exe"))) -and
        (-not (Test-Path -LiteralPath (Join-Path $Work "fresh\bin\mc-agent.installed"))))
}
else {
    Skip "injected commit-failure tests need -PreviousAssets"
}

# ------------------------------------------------------------------- upgrade

if ($PreviousAssets) {
    $null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-SkillDir", (Join-Path $Work "skills"), "-UpdateSkill")
    Check "upgrade updated the install manifest" ((Get-Content -LiteralPath $Manifest) -contains "version=$Version")
    Check "upgraded binary reports the new version" ((& $Binary version) -match "mc-agent $([regex]::Escape($Version)) ")
    Check "upgrade kept the previous binary" (Test-Path -LiteralPath (Join-Path $BinDir "mc-agent.previous.exe"))

    $mismatch = Join-Path $Work "staging-mismatch"
    New-Item -ItemType Directory -Path $mismatch -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $PreviousAssets "mc-agent-$PreviousVersion-windows-amd64.zip") -Destination (Join-Path $mismatch "mc-agent-$Version-windows-amd64.zip")
    New-Checksums $mismatch @("*.zip")
    $hashBefore = HashFile $Binary
    $null = Invoke-Installer @("-Version", $Version, "-FromDir", $mismatch, "-InstallDir", $BinDir, "-NoSkill")
    Check "mismatched binary version is refused" ($LASTEXITCODE -ne 0)
    Check "refused mismatch left the binary unchanged" ((HashFile $Binary) -eq $hashBefore)
}
else {
    Skip "version mismatch refusal needs -PreviousAssets"
}

# A modified managed binary must be refused without -Force; an empty manifest
# is not ownership. Then -Force replaces both.
Add-Content -LiteralPath $Binary -Value "modified"
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-NoSkill")
Check "modified managed binary is refused" ($LASTEXITCODE -ne 0)
Set-Content -LiteralPath $Manifest -Value "" -Encoding ASCII
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-NoSkill")
Check "empty manifest is not treated as ownership" ($LASTEXITCODE -ne 0)
$null = Invoke-Installer @("-Uninstall", "-InstallDir", $BinDir)
Check "uninstall with an empty manifest is refused" ($LASTEXITCODE -ne 0)
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-NoSkill", "-Force")
Check "-Force replaced the modified binary and manifest" ((& $Binary version) -match "mc-agent $([regex]::Escape($Version)) ")

# ---------------------------------------------------------------- skill safety

$skillEdit = Join-Path $Work "skills-edit"
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-SkillDir", $skillEdit)
$skillMd = Join-Path $skillEdit "minecraft-toolkit\SKILL.md"
Check "skill installed to the explicit directory" (Test-Path -LiteralPath $skillMd)
Add-Content -LiteralPath $skillMd -Value "`nuser edit marker"
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-SkillDir", $skillEdit)
Check "existing skill is not overwritten without -UpdateSkill" ($LASTEXITCODE -ne 0)
Check "user edit survived the refused install" ((Get-Content -LiteralPath $skillMd -Raw) -match "user edit marker")

if ($PreviousAssets) {
    $hashBefore = HashFile $Binary
    $null = Invoke-Installer @("-Version", $PreviousVersion, "-FromDir", $PreviousAssets, "-InstallDir", $BinDir, "-SkillDir", $skillEdit)
    Check "skill conflict aborts the upgrade before the binary changes" ($LASTEXITCODE -ne 0)
    Check "aborted upgrade left the binary unchanged" ((HashFile $Binary) -eq $hashBefore)
    Check "aborted upgrade left the manifest unchanged" ((Get-Content -LiteralPath $Manifest) -contains "version=$Version")
}
else {
    Skip "transaction-order test needs -PreviousAssets"
}

$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-SkillDir", $skillEdit, "-UpdateSkill")
Check "(-UpdateSkill) replaced the edited copy" (-not ((Get-Content -LiteralPath $skillMd -Raw) -match "user edit marker"))
Check "(-UpdateSkill) kept a backup" (([System.IO.Directory]::GetDirectories($skillEdit, "minecraft-toolkit.backup-*")).Count -gt 0)

$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-NoSkill")
Check "binary-only reinstall preserved the skill record" (($null -ne (Get-Content -LiteralPath $Manifest | Where-Object { $_ -like "skill.*.dir=*" -and $_ -match [regex]::Escape((Join-Path $skillEdit "minecraft-toolkit")) } | Select-Object -First 1)))
$null = Invoke-Installer @("-Uninstall", "-InstallDir", $BinDir, "-RemoveSkill")
Check "(-RemoveSkill) removed the preserved skill" (-not (Test-Path -LiteralPath (Join-Path $skillEdit "minecraft-toolkit")))

# Several explicit targets, including a relative path and non-ASCII/space names.
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-SkillDir", (Join-Path $Work "skillsA"))
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-SkillDir", (Join-Path $Work "skills dir\üñí"))
Push-Location $Work
try { $null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-SkillDir", ".\relskills") }
finally { Pop-Location }
$skillDirs = @(Get-Content -LiteralPath $Manifest | Where-Object { $_ -match '^skill\.\d+\.dir=' })
Check "three skill targets are recorded" ($skillDirs.Count -eq 3)
Check "non-ASCII/space skill path stored canonically" ([bool](($skillDirs -join "`n") -match [regex]::Escape((Join-Path $Work "skills dir\üñí\minecraft-toolkit"))))
Check "relative skill path stored as absolute" ([bool](($skillDirs -join "`n") -match [regex]::Escape((Join-Path $Work "relskills\minecraft-toolkit"))))
$null = Invoke-Installer @("-Uninstall", "-InstallDir", $BinDir, "-RemoveSkill")
Check "uninstall removed all recorded skill targets" (
    (-not (Test-Path -LiteralPath (Join-Path $Work "skillsA\minecraft-toolkit"))) -and
    (-not (Test-Path -LiteralPath (Join-Path $Work "skills dir\üñí\minecraft-toolkit"))) -and
    (-not (Test-Path -LiteralPath (Join-Path $Work "relskills\minecraft-toolkit"))))

# ------------------------------------------------------------- uninstall safety

$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-NoSkill")
Remove-Item -LiteralPath $Manifest -Force
$null = Invoke-Installer @("-Uninstall", "-InstallDir", $BinDir)
Check "uninstall without a manifest is refused" ($LASTEXITCODE -ne 0)
$null = Invoke-Installer @("-Uninstall", "-InstallDir", $BinDir, "-Force")
Check "(-Force) uninstall removed the unowned binary" (-not (Test-Path -LiteralPath $Binary))

# -PurgeState removes known files, preserves unrelated ones and keeps a
# nonempty directory.
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-NoSkill")
$stateMixed = Join-Path $Work "state-mixed"
New-Item -ItemType Directory -Path $stateMixed -Force | Out-Null
Set-Content -LiteralPath (Join-Path $stateMixed "daemon.json") -Value "{}"
Set-Content -LiteralPath (Join-Path $stateMixed "notes.txt") -Value "keep me"
$env:MC_AGENT_HOME = $stateMixed
$null = Invoke-Installer @("-Uninstall", "-InstallDir", $BinDir, "-PurgeState")
Check "purge removed the known state file" (-not (Test-Path -LiteralPath (Join-Path $stateMixed "daemon.json")))
Check "purge preserved an unrelated file" (Test-Path -LiteralPath (Join-Path $stateMixed "notes.txt"))
Check "purge kept the nonempty directory" (Test-Path -LiteralPath $stateMixed)

$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-NoSkill")
$stateEmpty = Join-Path $Work "state-empty"
New-Item -ItemType Directory -Path $stateEmpty -Force | Out-Null
Set-Content -LiteralPath (Join-Path $stateEmpty "daemon.json") -Value "{}"
$env:MC_AGENT_HOME = $stateEmpty
$null = Invoke-Installer @("-Uninstall", "-InstallDir", $BinDir, "-PurgeState")
Check "purge removed the empty product state directory" (-not (Test-Path -LiteralPath $stateEmpty))

$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-NoSkill")
$stateUnowned = Join-Path $Work "state-unowned"
New-Item -ItemType Directory -Path $stateUnowned -Force | Out-Null
Set-Content -LiteralPath (Join-Path $stateUnowned "notes.txt") -Value "keep me"
$env:MC_AGENT_HOME = $stateUnowned
$null = Invoke-Installer @("-Uninstall", "-InstallDir", $BinDir, "-PurgeState")
Check "purge-state refuses a directory without product ownership" ($LASTEXITCODE -ne 0)
Check "refused purge-state left the directory untouched" (Test-Path -LiteralPath (Join-Path $stateUnowned "notes.txt"))
$env:MC_AGENT_HOME = Join-Path $Work "home"

# A forged skill entry (not leaf minecraft-toolkit) is refused even with -Force.
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-NoSkill")
$evil = Join-Path $Work "evil"
New-Item -ItemType Directory -Path $evil -Force | Out-Null
Set-Content -LiteralPath (Join-Path $evil "file.txt") -Value "do not delete"
$manifestLines = @(
    "manifest_version=2", "product=mc-agent", "version=$Version", "platform=windows/amd64",
    "binary_sha256=$(HashFile $Binary)",
    "skill.1.dir=$evil", "skill.1.version=0.3.0", "skill.1.commit=deadbeef",
    "skill.1.tree_sha256=$(Get-TreeHash $evil)"
)
Set-Content -LiteralPath $Manifest -Value $manifestLines -Encoding UTF8
$null = Invoke-Installer @("-Uninstall", "-InstallDir", $BinDir, "-RemoveSkill", "-Force")
Check "forged non-skill manifest entry is never removed" (Test-Path -LiteralPath (Join-Path $evil "file.txt"))

# A forged skill entry that resolves through a link is refused.
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-NoSkill")
$realSkills = Join-Path $Work "real-skills"
New-Item -ItemType Directory -Path (Join-Path $realSkills "minecraft-toolkit") -Force | Out-Null
Set-Content -LiteralPath (Join-Path $realSkills "minecraft-toolkit\SKILL.md") -Value "real"
$linkSkills = Join-Path $Work "link-skills"
$linkCreated = $true
try { New-Item -ItemType Junction -Path $linkSkills -Target $realSkills -ErrorAction Stop | Out-Null }
catch { $linkCreated = $false }
if ($linkCreated) {
    $manifestLines = @(
        "manifest_version=2", "product=mc-agent", "version=$Version", "platform=windows/amd64",
        "binary_sha256=$(HashFile $Binary)",
        "skill.1.dir=$linkSkills\minecraft-toolkit", "skill.1.version=0.3.0", "skill.1.commit=deadbeef",
        "skill.1.tree_sha256=$(Get-TreeHash (Join-Path $realSkills 'minecraft-toolkit'))"
    )
    Set-Content -LiteralPath $Manifest -Value $manifestLines -Encoding UTF8
    $null = Invoke-Installer @("-Uninstall", "-InstallDir", $BinDir, "-RemoveSkill", "-Force")
    Check "linked skill path is never removed" (Test-Path -LiteralPath (Join-Path $realSkills "minecraft-toolkit\SKILL.md"))
}
else { Skip "junction/link test needs junction privileges" }

# -DryRun must not create install/skill/state directories or the profile entry.
$dry = Join-Path $Work "dry"
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", (Join-Path $dry "bin"), "-SkillDir", (Join-Path $dry "skills"), "-AddToPath", "-DryRun")
Check "dry-run exits successfully" ($LASTEXITCODE -eq 0)
Check "dry-run created nothing" (-not (Test-Path -LiteralPath $dry))

# A corrupted archive must fail verification and leave the install untouched.
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-NoSkill")
$corrupt = Join-Path $Work "staging-corrupt"
New-Item -ItemType Directory -Path $corrupt -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $Assets "mc-agent-$Version-windows-amd64.zip") -Destination (Join-Path $corrupt "mc-agent-$Version-windows-amd64.zip")
New-Checksums $corrupt @("*.zip")
Add-Content -LiteralPath (Join-Path $corrupt "mc-agent-$Version-windows-amd64.zip") -Value "corruption"
$hashBefore = HashFile $Binary
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $corrupt, "-InstallDir", $BinDir, "-NoSkill")
Check "corrupted archive is refused" ($LASTEXITCODE -ne 0)
Check "refused checksum left the binary unchanged" ((HashFile $Binary) -eq $hashBefore)

if ($script:Failures -ne 0) { Write-Error "$($script:Failures) check(s) failed"; exit 1 }
Write-Host "all release-install acceptance checks passed"
exit 0
