<#
    Remoter - publish a build to the hub, and update the hub machine's own node.

        .\deploy\deploy-hub.ps1              # hub, web client, downloads, the hub machine's node
        .\deploy\deploy-hub.ps1 -SkipWeb     # without the web client
        .\deploy\deploy-hub.ps1 -SkipNode    # without the hub machine's node

    Uploads .\dist and web\build over SSH (Host alias "hub" in ~/.ssh/config, or -SshHost),
    then runs install-hub.sh and install-node.sh with sudo - you will be asked
    for the hub machine's password once. The downloads (/dl/remoter-node.exe,
    /dl/install-node.ps1, /dl/remoter.apk) are what a new device installs from.
#>

[CmdletBinding()]
param(
    [string]$SshHost = 'hub',
    [string]$NodeUser = '',   # default: the user the SSH alias logs in as
    [switch]$SkipWeb,
    [switch]$SkipNode
)

$ErrorActionPreference = 'Stop'

$Root  = Split-Path -Parent $PSScriptRoot
$Dist  = Join-Path $Root 'dist'
$Stage = Join-Path $env:TEMP 'remoter-hub-stage'

function Ok($m) { Write-Host "  [ok]   $m" -ForegroundColor Green }

if (-not (Test-Path (Join-Path $Dist 'remoter-hub'))) { throw 'dist\remoter-hub missing - run .\deploy\build.ps1 first' }

Remove-Item $Stage -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $Stage, (Join-Path $Stage 'dl') | Out-Null

Copy-Item (Join-Path $Dist 'remoter-hub') $Stage
Copy-Item (Join-Path $PSScriptRoot 'install-hub.sh') $Stage
foreach ($f in 'remoter-node.exe', 'remoter.apk') {
    $p = Join-Path $Dist $f
    if (Test-Path $p) { Copy-Item $p (Join-Path $Stage 'dl') }
}
Copy-Item (Join-Path $PSScriptRoot 'install-node.ps1') (Join-Path $Stage 'dl')
if (-not $SkipNode) {
    # The hub machine as a target (Phase 2): the Linux node, its first-install action list
    # and its installer. The action list only seeds a fresh install; an
    # existing ~/.config/remoter/actions.yaml there is never overwritten.
    $node = Join-Path $Stage 'node'
    New-Item -ItemType Directory -Force -Path $node | Out-Null
    Copy-Item (Join-Path $Dist 'remoter-node-linux-amd64') (Join-Path $node 'remoter-node')
    Copy-Item (Join-Path $PSScriptRoot 'actions-hub.yaml') (Join-Path $node 'actions.yaml')
    Copy-Item (Join-Path $PSScriptRoot 'install-node.sh') $node
}
if (-not $SkipWeb) {
    $web = Join-Path $Root 'web\build'
    if (-not (Test-Path (Join-Path $web 'index.html'))) { throw 'web\build missing - run .\deploy\build.ps1 first' }
    Copy-Item $web (Join-Path $Stage 'web') -Recurse
}
Ok "staged $Stage"

if (-not $NodeUser) { $NodeUser = (ssh $SshHost whoami).Trim() }
ssh $SshHost 'rm -rf /tmp/remoter-stage && mkdir -p /tmp/remoter-stage'
if ($LASTEXITCODE -ne 0) { throw "ssh $SshHost failed" }
scp -q -r "$Stage\*" "${SshHost}:/tmp/remoter-stage/"
if ($LASTEXITCODE -ne 0) { throw 'upload failed' }
Ok "uploaded to ${SshHost}:/tmp/remoter-stage"

# The scripts were staged from a Windows checkout: strip CRs before sh sees
# them. One ssh -t session (sudo needs a terminal), so the password is asked
# for once, for both installs.
$crlf = "sed -i 's/\r`$//'"
$cmd = "cd /tmp/remoter-stage && $crlf install-hub.sh && sudo sh install-hub.sh /tmp/remoter-stage"
if (-not $SkipNode) {
    $cmd += " && $crlf node/install-node.sh node/actions.yaml && sudo sh node/install-node.sh /tmp/remoter-stage/node $NodeUser"
}
ssh -t $SshHost $cmd
if ($LASTEXITCODE -ne 0) { throw 'install on the hub failed' }
