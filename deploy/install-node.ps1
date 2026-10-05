<#
    Remoter - install or update the node on a Windows machine.

        .\deploy\install-node.ps1                       # install / update
        .\deploy\install-node.ps1 -Uninstall            # remove the service
        .\deploy\install-node.ps1 -Hub https://<hub>.<tailnet>.ts.net   # first install

    Self-elevates (one UAC prompt). Idempotent: re-run it to update.

    What it does, in order:
      1. retires the old agent (scheduled task, remote-mode arming)
      2. installs remoter-node.exe to Program Files, where only admins can write -
         the service runs as SYSTEM, so a user-writable binary would be a
         privilege escalation waiting to happen
      3. locks %ProgramData%\Remoter down the same way, and the token tighter
      4. registers the RemoterNode boot-time service
      5. enrols with the hub, which then accepts the node once you approve it
      6. allows inbound 8737 from the hub's address only
      7. puts Tailscale in unattended mode, so the tailnet - and therefore this
         machine - comes up after a reboot even before anyone signs in

    The binary is taken from next to this script, then from ..\dist, and finally
    downloaded from the hub.
#>

[CmdletBinding()]
param(
    [string]$Hub = '',
    [string]$Binary = '',
    [switch]$Uninstall
)

$ErrorActionPreference = 'Stop'

# --- self-elevate -----------------------------------------------------------
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
           ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    $a = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-NoExit', '-File', "`"$PSCommandPath`"")
    if ($Hub)       { $a += @('-Hub', $Hub) }
    if ($Binary)    { $a += @('-Binary', "`"$Binary`"") }
    if ($Uninstall) { $a += '-Uninstall' }
    Start-Process powershell.exe -Verb RunAs -ArgumentList $a
    return
}

function Ok($m)   { Write-Host "  [ok]   $m" -ForegroundColor Green }
function Skip($m) { Write-Host "  [skip] $m" -ForegroundColor DarkGray }
function Warn($m) { Write-Host "  [warn] $m" -ForegroundColor Yellow }
function Head($m) { Write-Host "`n=== $m" -ForegroundColor Cyan }

$InstallDir = Join-Path $env:ProgramFiles 'Remoter'
$Exe        = Join-Path $InstallDir 'remoter-node.exe'
$DataDir    = Join-Path $env:ProgramData 'Remoter'
$Service    = 'RemoterNode'
$RuleName   = 'Remoter Node (hub only)'
$Tailscale  = Join-Path $env:ProgramFiles 'Tailscale\tailscale.exe'

# The hub: as given, or - on an update - the one this node already answers to.
if (-not $Hub) {
    $existing = Join-Path $DataDir 'config.json'
    if (Test-Path $existing) { $Hub = (Get-Content $existing -Raw | ConvertFrom-Json).hub.url }
}
if (-not $Hub -and -not $Uninstall) { throw 'First install: pass the hub, e.g. -Hub https://<hub>.<tailnet>.ts.net' }
$Hub = "$Hub".TrimEnd('/')

# Well-known SIDs, not names: group names are localised ("Rendszergazdák"), SIDs
# are not.
$SidSystem = '*S-1-5-18'
$SidAdmins = '*S-1-5-32-544'
$SidUsers  = '*S-1-5-32-545'

function Invoke-Native {
    param([scriptblock]$Command, [string]$What)
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { $out = & $Command 2>&1 } finally { $ErrorActionPreference = $prev }
    if ($LASTEXITCODE -ne 0) { throw "$What failed (exit $LASTEXITCODE): $out" }
    $out
}

function Stop-NodeService {
    $svc = Get-Service $Service -ErrorAction SilentlyContinue
    if ($svc -and $svc.Status -ne 'Stopped') {
        Stop-Service $Service -Force
        $svc.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(15))
        Ok 'stopped the running node'
    }
    # Helpers die with the service (job object), but a wedged one must not keep
    # the binary locked.
    Get-Process remoter-node -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
}

# ============================ UNINSTALL ====================================
if ($Uninstall) {
    Head 'Removing the Remoter node'
    Stop-NodeService
    if (Test-Path $Exe) { & $Exe uninstall } else { Skip 'binary not present' }
    Get-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
    Ok 'firewall rule removed'
    Write-Host "`n  Config and token are kept in $DataDir. Delete that folder to forget this node entirely." -ForegroundColor White
    Write-Host '  Remove it on the hub as well: sudo remoter-hub revoke <name>' -ForegroundColor White
    return
}

# ============================ INSTALL ======================================
Head 'Retiring the old agent'
if (Get-ScheduledTask -TaskName 'Remoter Agent' -ErrorAction SilentlyContinue) {
    Stop-ScheduledTask -TaskName 'Remoter Agent' -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskName 'Remoter Agent' -Confirm:$false
    Ok "removed the 'Remoter Agent' scheduled task"
} else { Skip 'no old scheduled task' }
Get-Process remoter-agent -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue

# remote-mode.ps1 -On switched power schemes; give the owner theirs back. From
# now on arming is done by the node, from the app.
$oldArm = Join-Path $DataDir 'remote-mode.json'
if (Test-Path $oldArm) {
    $state = Get-Content $oldArm -Raw | ConvertFrom-Json
    if ($state.previousScheme) { powercfg /setactive $state.previousScheme; Ok 'restored the power scheme the old arming replaced' }
    Remove-Item $oldArm -Force
}
foreach ($stale in 'remoter-agent.exe', 'agent.log', 'agent.log.elevated', 'agent.err', 'web') {
    $p = Join-Path $DataDir $stale
    if (Test-Path $p) { Remove-Item $p -Recurse -Force -ErrorAction SilentlyContinue; Ok "removed old $stale" }
}

Head 'Binary'
if (-not $Binary) {
    foreach ($c in (Join-Path $PSScriptRoot 'remoter-node.exe'), (Join-Path (Split-Path $PSScriptRoot -Parent) 'dist\remoter-node.exe')) {
        if (Test-Path $c) { $Binary = $c; break }
    }
}
if (-not $Binary) {
    $Binary = Join-Path $env:TEMP 'remoter-node.exe'
    Invoke-WebRequest -UseBasicParsing -Uri "$Hub/dl/remoter-node.exe" -OutFile $Binary
    Ok "downloaded from $Hub"
}
Stop-NodeService
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item $Binary $Exe -Force
Ok "installed $Exe"

Head 'Configuration'
New-Item -ItemType Directory -Force -Path $DataDir, (Join-Path $DataDir 'logs') | Out-Null

# Owned by Administrators, writable only by SYSTEM and Administrators. The
# service writes logs here as SYSTEM; if users could write too, a planted link
# would turn those writes into a privilege escalation.
Invoke-Native { icacls $DataDir /setowner $SidAdmins /T /C /Q } 'icacls setowner' | Out-Null
# The folder gets the inheritable ACL; everything inside is reset to inherit
# it. (Applying the folder's inheritance flags to files with /T fails part-way
# and leaves those files readable by nobody but their owner.)
Invoke-Native { icacls $DataDir /inheritance:r /grant:r "${SidSystem}:(OI)(CI)F" "${SidAdmins}:(OI)(CI)F" "${SidUsers}:(OI)(CI)RX" /Q } 'icacls grant' | Out-Null
if (Get-ChildItem $DataDir -Force) {
    Invoke-Native { icacls "$DataDir\*" /reset /T /C /Q } 'icacls reset' | Out-Null
}
Ok "${DataDir}: SYSTEM and Administrators write, Users read"

$cfgPath = Join-Path $DataDir 'config.json'
$cfg = if (Test-Path $cfgPath) { Get-Content $cfgPath -Raw | ConvertFrom-Json } else { [pscustomobject]@{} }
# The node binds the tailnet address itself, and waits for it at boot.
$cfg | Add-Member -NotePropertyName listen -NotePropertyValue @('tailscale:8737') -Force
foreach ($obsolete in 'ntfyServer', 'ntfyTopic', 'webDir', 'sunshine') { $cfg.PSObject.Properties.Remove($obsolete) }
if (-not $cfg.PSObject.Properties['actionsFile']) { $cfg | Add-Member -NotePropertyName actionsFile -NotePropertyValue 'actions.yaml' }
if (-not $cfg.PSObject.Properties['console']) { $cfg | Add-Member -NotePropertyName console -NotePropertyValue ([pscustomobject]@{ enabled = $true }) }
# UTF-8 without a BOM: Go's JSON parser tolerates one now, but nothing needs it.
[IO.File]::WriteAllText($cfgPath, ($cfg | ConvertTo-Json -Depth 6), (New-Object Text.UTF8Encoding $false))
Ok 'config.json: listen on the tailnet address'

$actions = Join-Path $DataDir 'actions.yaml'
if (-not (Test-Path $actions)) {
    $example = Join-Path (Split-Path $PSScriptRoot -Parent) 'agent\actions.example.yaml'
    if (Test-Path $example) { Copy-Item $example $actions; Ok 'seeded actions.yaml' }
}

# Creates the token on first run. The token is the hub's key to this machine:
# nobody but SYSTEM and Administrators may read it.
& $Exe print-token | Out-Null
$token = Join-Path $DataDir 'token'
Invoke-Native { icacls $token /inheritance:r /grant:r "${SidSystem}:F" "${SidAdmins}:F" /C /Q } 'icacls token' | Out-Null
Ok 'token readable by SYSTEM and Administrators only'

Head 'Service'
Invoke-Native { & $Exe install } 'service install' | Out-Null
Ok "registered $Service (automatic, restarts on failure)"

Head 'Hub'
$enrol = Invoke-Native { & $Exe enroll -hub $Hub } 'enrolment'
$enrol | ForEach-Object { Write-Host "  $_" }

# The hub's address is now in config.json; scope the firewall to it.
$cfg = Get-Content $cfgPath -Raw | ConvertFrom-Json
$hubAddr = @($cfg.hub.addrs)[0]
Get-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
New-NetFirewallRule -DisplayName $RuleName -Direction Inbound -Action Allow -Protocol TCP -LocalPort 8737 `
    -RemoteAddress $hubAddr -Program $Exe -Profile Any | Out-Null
Ok "firewall: inbound 8737 from $hubAddr only"
$off = Get-NetFirewallProfile | Where-Object { -not $_.Enabled } | ForEach-Object Name
if ($off) {
    Warn ("Windows Firewall is OFF for: {0}. The node still refuses everything but the hub itself," -f ($off -join ', '))
    Warn '       but the firewall is the second lock. Turn it back on in Windows Security.'
}

Head 'Tailscale'
if (Test-Path $Tailscale) {
    $prev = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    & $Tailscale set --unattended=true 2>&1 | Out-Null
    $code = $LASTEXITCODE
    $ErrorActionPreference = $prev
    if ($code -eq 0) { Ok 'unattended mode on: the tailnet comes up at boot, before sign-in' }
    else { Warn 'could not enable unattended mode - in the Tailscale menu: Preferences > Run unattended' }
} else { Warn 'Tailscale not found - install it, or this machine is unreachable' }

Head 'Starting'
Start-Service $Service
Start-Sleep -Seconds 3
$svc = Get-Service $Service
if ($svc.Status -eq 'Running') { Ok 'RemoterNode is running' }
else { Warn "RemoterNode is $($svc.Status) - see $DataDir\logs\node.log" }
Get-Content (Join-Path $DataDir 'logs\node.log') -Tail 4 -ErrorAction SilentlyContinue | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkGray }

Write-Host "`n  Open $Hub to use this machine." -ForegroundColor White
if ($enrol -match 'pending') {
    Write-Host '  It is waiting for approval: approve it from a device you already use,' -ForegroundColor White
    Write-Host "  or on the hub:  sudo remoter-hub approve $env:COMPUTERNAME" -ForegroundColor White
}
