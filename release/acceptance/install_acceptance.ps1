# Release-installed acceptance for Windows amd64. Runs the real install script
# against a staged release directory, then exercises version/doctor/upgrade/
# uninstall/skill safety.
#
#   pwsh release/acceptance/install_acceptance.ps1 -Assets dist -Version 0.5.0 -Work $env:TEMP\mc-agent-acceptance
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Assets,
    [Parameter(Mandatory = $true)][string]$Version,
    [Parameter(Mandatory = $true)][string]$Work,
    [string]$Installer = "install/install.ps1"
)

$ErrorActionPreference = "Stop"
$Assets = (Resolve-Path -LiteralPath $Assets).Path
if (Test-Path -LiteralPath $Work) { Remove-Item -LiteralPath $Work -Recurse -Force }
New-Item -ItemType Directory -Path (Join-Path $Work "bin"), (Join-Path $Work "home"), (Join-Path $Work "skills"), (Join-Path $Work "staging-next") -Force | Out-Null
$BinDir = Join-Path $Work "bin"
$Binary = Join-Path $BinDir "mc-agent.exe"
$Manifest = Join-Path $BinDir "mc-agent.installed"
$script:Failures = 0

function Check([string]$Name, [bool]$Ok) {
    if ($Ok) { Write-Host "PASS  $Name" }
    else { Write-Host "FAIL  $Name" -ForegroundColor Red; $script:Failures++ }
}

function HashFile([string]$Path) { (Get-FileHash -Algorithm SHA256 -LiteralPath $Path).Hash.ToLowerInvariant() }

function Invoke-Installer([string[]]$Arguments) {
    & pwsh -NoProfile -File $Installer @Arguments
    return $LASTEXITCODE
}

$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-NoSkill")
if ($LASTEXITCODE -ne 0) { throw "baseline install failed" }
$versionOutput = & $Binary version
Check "installed binary reports version $Version" ($versionOutput -match "mc-agent $Version")

$env:MC_AGENT_HOME = Join-Path $Work "home"
$null = & $Binary daemon start --fake
$capabilities = (& $Binary capabilities | Out-String)
Check "capabilities lists state through the daemon" ($capabilities -match '"state"')
$state = (& $Binary state | Out-String)
Check "state answers through the fake mod" ($state -match '"stub":true')
$doctor = (& $Binary doctor | Out-String)
Check "doctor reports the installed version" ($doctor -match ('"check":"version","detail":"mc-agent ' + [regex]::Escape($Version)))
Check "doctor sees the running daemon" ($doctor -match '"check":"daemon","detail":"running"')
$null = & $Binary daemon stop

$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-SkillDir", (Join-Path $Work "skills"))
Check "skill installed to the explicit directory" (Test-Path -LiteralPath (Join-Path $Work "skills\minecraft-toolkit\SKILL.md"))
Add-Content -LiteralPath (Join-Path $Work "skills\minecraft-toolkit\SKILL.md") -Value "`nuser edit marker"
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-SkillDir", (Join-Path $Work "skills"))
Check "existing skill is not overwritten without -UpdateSkill" ($LASTEXITCODE -ne 0)
Check "user edit survived the refused install" ((Get-Content -LiteralPath (Join-Path $Work "skills\minecraft-toolkit\SKILL.md") -Raw) -match "user edit marker")
$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-SkillDir", (Join-Path $Work "skills"), "-UpdateSkill")
Check "(-UpdateSkill) replaced the edited copy" (-not ((Get-Content -LiteralPath (Join-Path $Work "skills\minecraft-toolkit\SKILL.md") -Raw) -match "user edit marker"))
Check "(-UpdateSkill) kept a backup" (([System.IO.Directory]::GetDirectories((Join-Path $Work "skills"), "minecraft-toolkit.backup-*")).Count -gt 0)

$nextVersion = "$Version-upgradetest"
$sourceArchive = (Get-ChildItem -LiteralPath $Assets -Filter "mc-agent-$Version-windows-amd64.zip").FullName
Copy-Item -LiteralPath $sourceArchive -Destination (Join-Path $Work "staging-next\mc-agent-$nextVersion-windows-amd64.zip")
$hash = HashFile (Join-Path $Work "staging-next\mc-agent-$nextVersion-windows-amd64.zip")
Set-Content -LiteralPath (Join-Path $Work "staging-next\checksums.txt") -Value "$hash  mc-agent-$nextVersion-windows-amd64.zip"
$null = Invoke-Installer @("-Version", $nextVersion, "-FromDir", (Join-Path $Work "staging-next"), "-InstallDir", $BinDir, "-NoSkill")
Check "upgrade updated the install manifest" ((Get-Content -LiteralPath $Manifest | Select-String "^version=$nextVersion$") -ne $null)
Check "upgrade kept the previous binary" (Test-Path -LiteralPath (Join-Path $BinDir "mc-agent.previous.exe"))
Check "upgraded binary still runs" ((& $Binary version) -match "mc-agent $Version")

$beforeHash = HashFile $Binary
Add-Content -LiteralPath (Join-Path $Work "staging-next\mc-agent-$nextVersion-windows-amd64.zip") -Value "corruption"
$null = Invoke-Installer @("-Version", $nextVersion, "-FromDir", (Join-Path $Work "staging-next"), "-InstallDir", $BinDir, "-NoSkill")
Check "corrupted archive is refused" ($LASTEXITCODE -ne 0)
Check "refused upgrade left the binary unchanged" ((HashFile $Binary) -eq $beforeHash)

Set-Content -LiteralPath (Join-Path $Work "home\sentinel") -Value "keep"
$null = Invoke-Installer @("-Uninstall", "-InstallDir", $BinDir)
Check "uninstall removed the binary" (-not (Test-Path -LiteralPath $Binary))
Check "uninstall preserved the state directory" (Test-Path -LiteralPath (Join-Path $Work "home\sentinel"))
Check "uninstall preserved the skill unless asked" (Test-Path -LiteralPath (Join-Path $Work "skills\minecraft-toolkit"))

$null = Invoke-Installer @("-Version", $Version, "-FromDir", $Assets, "-InstallDir", $BinDir, "-SkillDir", (Join-Path $Work "skills"), "-UpdateSkill")
$null = Invoke-Installer @("-Uninstall", "-InstallDir", $BinDir, "-RemoveSkill", "-PurgeState")
Check "second uninstall removed the binary" (-not (Test-Path -LiteralPath $Binary))
Check "(-RemoveSkill) removed the unchanged skill" (-not (Test-Path -LiteralPath (Join-Path $Work "skills\minecraft-toolkit")))
Check "(-PurgeState) removed the state directory" (-not (Test-Path -LiteralPath (Join-Path $Work "home")))

if ($script:Failures -ne 0) { Write-Error "$($script:Failures) check(s) failed"; exit 1 }
Write-Host "all release-install acceptance checks passed"
