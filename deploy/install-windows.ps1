<#
    Remoter — Phase 0 host bootstrap (Windows). Historical: superseded by
    install-node.ps1, which is all a device needs to become controllable. Kept
    for its SSH and Wi-Fi hardening.

    Note that its power section disables sleep on the ACTIVE scheme for good,
    which defeats the node's arm/disarm model; prefer arming from the app.

    Idempotent: safe to re-run. Self-elevates via UAC if needed.

        powershell -ExecutionPolicy Bypass -File .\deploy\install-windows.ps1

    Does NOT do (deliberately, it needs your input):
      - tailscale up --unattended      (interactive browser login)
#>

[CmdletBinding()]
param(
    # Skip the winget installs, only apply power/ssh settings.
    [switch]$NoInstall,

    # Scope the inbound SSH rule to the tailnet (100.64.0.0/10). Opt-in: it
    # removes LAN SSH access, so if Tailscale is down you lose the console too.
    [switch]$HardenSsh
)

$ErrorActionPreference = 'Stop'

# --- self-elevate -----------------------------------------------------------
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$isAdmin  = ([Security.Principal.WindowsPrincipal]$identity).IsInRole(
                [Security.Principal.WindowsBuiltInRole]::Administrator)

if (-not $isAdmin) {
    Write-Host "Not elevated - relaunching through UAC..." -ForegroundColor Yellow
    $argList = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-NoExit',
                 '-File', "`"$PSCommandPath`"")
    if ($NoInstall) { $argList += '-NoInstall' }
    Start-Process -FilePath 'powershell.exe' -Verb RunAs -ArgumentList $argList
    return
}

function Step($msg) { Write-Host "`n=== $msg" -ForegroundColor Cyan }
function Ok($msg)   { Write-Host "  [ok]   $msg" -ForegroundColor Green }
function Skip($msg) { Write-Host "  [skip] $msg" -ForegroundColor DarkGray }
function Warn($msg) { Write-Host "  [warn] $msg" -ForegroundColor Yellow }

# A pending reboot is a normal, recoverable state - it must not abort the rest of
# the script, since power and Wi-Fi settings are unrelated to servicing.
$script:RebootNeeded = $false
function Test-RebootPending {
    (Test-Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending') -or
    (Test-Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired')
}

# Run a section; a failure warns and continues rather than killing the script.
function Section($name, [scriptblock]$body) {
    Step $name
    try { & $body } catch { Warn "section failed: $($_.Exception.Message)" }
}

if (Test-RebootPending) {
    Warn 'A reboot is already pending. Feature installs will stage but not register until you restart.'
    $script:RebootNeeded = $true
}

# --- 1. OpenSSH Server ------------------------------------------------------
Section 'OpenSSH Server (Tier 2)' {
    $cap = Get-WindowsCapability -Online -Name 'OpenSSH.Server*'

    switch -Regex ($cap.State) {
        'NotPresent' {
            Add-WindowsCapability -Online -Name $cap.Name | Out-Null
            Ok "installed $($cap.Name)"
        }
        'Installed' { Skip "$($cap.Name) already installed" }
        default     { Warn "$($cap.Name) state = $($cap.State)" }
    }

    # Re-read: the install may have landed as InstallPending behind a queued reboot.
    $cap = Get-WindowsCapability -Online -Name 'OpenSSH.Server*'
    $sshd = Get-Service -Name sshd -ErrorAction SilentlyContinue

    if (-not $sshd) {
        # The service only registers once servicing finishes. Do not treat as fatal.
        $script:RebootNeeded = $true
        Warn "sshd service not registered yet (capability state: $($cap.State))."
        Warn 'Reboot, then re-run this script - it will pick up where it left off.'
        return
    }

    Set-Service -Name sshd -StartupType Automatic
    if ($sshd.Status -ne 'Running') { Start-Service sshd; Ok 'sshd started' }
    else { Skip 'sshd already running' }

    # Windows' OpenSSH package normally adds an "any address" rule. Once Tailscale is
    # up you want SSH reachable on the tailnet only - tighten, do not widen.
    if (-not (Get-NetFirewallRule -Name 'Remoter-SSH-Tailscale' -ErrorAction SilentlyContinue)) {
        Warn 'Review firewall: prefer restricting sshd to the Tailscale interface once tailscaled is up.'
    }
}

# --- 1b. Restrict SSH to the tailnet (opt-in) -------------------------------
Section 'SSH exposure' {
    $listen = Get-NetTCPConnection -State Listen -LocalPort 22 -ErrorAction SilentlyContinue
    if (-not $listen) { Skip 'nothing listening on 22'; return }

    $anyAddr = $listen | Where-Object { $_.LocalAddress -in '0.0.0.0', '::' }
    if ($anyAddr) { Warn 'sshd is listening on ALL interfaces (0.0.0.0 / ::), not just the tailnet.' }

    $rules = Get-NetFirewallRule -Enabled True -Direction Inbound -ErrorAction SilentlyContinue |
             Where-Object { ($_ | Get-NetFirewallPortFilter).LocalPort -eq 22 }

    if (-not $HardenSsh) {
        foreach ($r in $rules) { Warn "rule '$($r.DisplayName)' profile=$($r.Profile) allows inbound 22" }
        Warn 'Re-run with -HardenSsh to scope these to 100.64.0.0/10 (tailnet only).'
        return
    }

    foreach ($r in $rules) {
        $scope = ($r | Get-NetFirewallAddressFilter).RemoteAddress
        if ($scope -eq '100.64.0.0/10') {
            Skip "rule '$($r.DisplayName)' already tailnet-scoped"
        } else {
            Set-NetFirewallRule -Name $r.Name -RemoteAddress '100.64.0.0/10'
            Ok "rule '$($r.DisplayName)' scoped to 100.64.0.0/10"
        }
    }
    Warn 'SSH is now tailnet-only. If Tailscale is down you cannot SSH in from the LAN.'
}

# --- 2. Packages ------------------------------------------------------------
Section 'Packages (Tailscale)' {
    if ($NoInstall) { Skip 'package installs (-NoInstall)'; return }

    # winget list is NOT authoritative: Tailscale registers outside winget's
    # tracking and reports as absent while its service is running. Check for the
    # thing itself first, and only fall back to winget for the query.
    $pkgs = @(
        @{ Id = 'tailscale.tailscale'; Service = 'Tailscale' }
    )

    foreach ($p in $pkgs) {
        $present = [bool](Get-Service -Name $p.Service -ErrorAction SilentlyContinue)
        if (-not $present) {
            $present = [bool](winget list --id $p.Id --exact 2>$null | Select-String $p.Id)
        }

        if ($present) {
            Skip "$($p.Id) already installed"
        } else {
            winget install --id $p.Id --exact --accept-package-agreements --accept-source-agreements
            if ($LASTEXITCODE -eq 0) { Ok "$($p.Id) installed" } else { Warn "$($p.Id) failed (exit $LASTEXITCODE)" }
        }
    }
}

# --- 3. Power: this is a laptop, so this section is the availability story ---
Section 'Power (never sleep, lid stays awake)' {
    powercfg /change standby-timeout-ac 0
    powercfg /change standby-timeout-dc 0
    powercfg /change hibernate-timeout-ac 0
    powercfg /change monitor-timeout-ac 10      # screen may sleep; the machine may not
    Ok 'sleep/hibernate timeouts disabled'

    # Lid close on AC -> Do nothing (0). On battery, leave the default.
    $SUB_BUTTONS = '4f971e89-eebd-4455-a8de-9e59040e7347'
    $LIDACTION   = '5ca83367-6e45-459f-a27b-476b1d01c936'
    powercfg /setacvalueindex SCHEME_CURRENT $SUB_BUTTONS $LIDACTION 0
    powercfg /setactive SCHEME_CURRENT
    Ok 'lid close on AC -> do nothing'
}

# --- 4. Wi-Fi power saving: the top cause of "it was reachable an hour ago" --
Section 'Wi-Fi adapter power management' {
    $wifi = Get-NetAdapter -Physical | Where-Object { $_.InterfaceDescription -match 'Wireless|Wi-Fi|WLAN' -and $_.Status -eq 'Up' }
    if (-not $wifi) { Warn 'no wireless adapter matched - nothing to do'; return }

    foreach ($nic in $wifi) {
        $done = $false

        # Path 1: the WMI toggle. Not every driver exposes it - the MediaTek
        # MT7921 on this host does not, which is why this used to print nothing.
        try {
            $pm = Get-CimInstance -Namespace root/wmi -ClassName MSPower_DeviceEnable -ErrorAction Stop |
                  Where-Object { $_.InstanceName -like "$($nic.PnPDeviceID)*" }
            foreach ($p in $pm) {
                if ($p.Enable) { $p.Enable = $false; Set-CimInstance -InputObject $p; Ok "$($nic.Name): 'turn off to save power' disabled (WMI)" }
                else { Skip "$($nic.Name): already disabled (WMI)" }
                $done = $true
            }
        } catch { }

        if ($done) { continue }

        # Path 2: PnPCapabilities in the driver's class key. 24 (0x18) = do not
        # allow the machine to power the device down. Works where WMI does not.
        try {
            $classKey = 'HKLM:\SYSTEM\CurrentControlSet\Control\Class\{4d36e972-e325-11ce-bfc1-08002be10318}'
            $match = Get-ChildItem $classKey -ErrorAction Stop | Where-Object {
                (Get-ItemProperty $_.PSPath -Name 'NetCfgInstanceId' -ErrorAction SilentlyContinue).NetCfgInstanceId -eq $nic.InterfaceGuid
            }
            if (-not $match) { Warn "$($nic.Name): no driver class key found - set it in Device Manager > adapter > Power Management."; continue }

            foreach ($k in $match) {
                $cur = (Get-ItemProperty $k.PSPath -Name 'PnPCapabilities' -ErrorAction SilentlyContinue).PnPCapabilities
                if ($cur -eq 24) {
                    Skip "$($nic.Name): PnPCapabilities already 24"
                } else {
                    Set-ItemProperty $k.PSPath -Name 'PnPCapabilities' -Value 24 -Type DWord
                    Ok "$($nic.Name): PnPCapabilities set to 24 (takes effect after reboot or adapter reset)"
                    $script:RebootNeeded = $true
                }
            }
        } catch {
            Warn "$($nic.Name): could not disable power management - do it in Device Manager > adapter > Power Management."
        }
    }
}

# --- 5. Report: Modern Standby decides whether any of the above is enough ----
Section 'Sleep-state report (read this)' {
    $sleepStates = powercfg /a
    $sleepStates | Select-String -Pattern 'Standby|Hibernate|available' | ForEach-Object { "  $_" }
    if ($sleepStates -match 'S0 Low Power Idle') {
        Warn 'Modern Standby (S0) host: the machine can drop the network while "awake". Verify reachability after 30+ idle minutes before trusting it.'
    }
}

# --- 6. What is left for you ------------------------------------------------
Step 'Manual steps remaining'
if ($script:RebootNeeded -or (Test-RebootPending)) {
    Write-Host '  - REBOOT, then re-run this script. Servicing is incomplete until you do.' -ForegroundColor Yellow
    Write-Host '    After the reboot, check `Get-Service sshd` WITHOUT logging in elsewhere -' -ForegroundColor Yellow
    Write-Host '    it should already be Running. Same question tailscale --unattended answers.' -ForegroundColor Yellow
}
@(
    'tailscale up --unattended        <- REQUIRED, or a reboot locks you out',
    'Lid closed + no external monitor = no display to capture. Keep the lid open,',
    '  or install a virtual display driver.',
    'Lenovo Vantage -> battery conservation mode (~60%) if it lives on AC',
    'Then: .\deploy\install-node.ps1 to make this PC controllable through the hub'
) | ForEach-Object { Write-Host "  - $_" }

Write-Host "`nDone." -ForegroundColor Green
