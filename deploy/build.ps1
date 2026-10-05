<#
    Remoter - build everything into .\dist.

        .\deploy\build.ps1               # node (Windows), hub (Linux), web client
        .\deploy\build.ps1 -Android      # also the APK
        .\deploy\build.ps1 -CrossCheck   # also verify the portability matrix
        .\deploy\build.ps1 -Deploy       # then publish to the hub (deploy-hub.ps1)

    Nothing here installs anything. The hub is updated by deploy-hub.ps1; a
    Windows node by install-node.ps1, which needs elevation because the node runs
    as a SYSTEM service.
#>

[CmdletBinding()]
param(
    [switch]$SkipWeb,
    [switch]$Android,
    [switch]$CrossCheck,
    [switch]$Deploy
)

$ErrorActionPreference = 'Stop'

$Root = Split-Path -Parent $PSScriptRoot
$Dist = Join-Path $Root 'dist'

function Step($m) { Write-Host "`n=== $m" -ForegroundColor Cyan }
function Ok($m)   { Write-Host "  [ok]   $m" -ForegroundColor Green }

# Native tools write progress and warnings to stderr, which PowerShell turns
# into terminating errors under -ErrorActionPreference Stop. The exit code is
# the only reliable signal, so check that instead.
function Invoke-Native {
    param([Parameter(Mandatory)][scriptblock]$Command, [Parameter(Mandatory)][string]$What)
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { & $Command } finally { $ErrorActionPreference = $prev }
    if ($LASTEXITCODE -ne 0) { throw "$What failed (exit $LASTEXITCODE)" }
}

function Build-Go($goos, $goarch, $pkg, $out) {
    $env:GOOS = $goos; $env:GOARCH = $goarch; $env:CGO_ENABLED = '0'
    try { Invoke-Native { go build -trimpath -ldflags '-s -w' -o $out $pkg } "go build $pkg ($goos/$goarch)" }
    finally { $env:GOOS = ''; $env:GOARCH = ''; $env:CGO_ENABLED = '' }
    Ok ("{0} ({1:N1} MB)" -f (Split-Path $out -Leaf), ((Get-Item $out).Length / 1MB))
}

New-Item -ItemType Directory -Force -Path $Dist | Out-Null

# --- Go ---------------------------------------------------------------------
Step 'Go'
Push-Location (Join-Path $Root 'agent')
try {
    Invoke-Native { go vet ./... } 'go vet'
    Ok 'vet clean'
    Invoke-Native { go test ./... } 'go test'
    Ok 'tests pass'

    Build-Go windows amd64 ./cmd/remoter-node (Join-Path $Dist 'remoter-node.exe')
    Build-Go linux   amd64 ./cmd/remoter-hub  (Join-Path $Dist 'remoter-hub')
    Build-Go linux   amd64 ./cmd/remoter-node (Join-Path $Dist 'remoter-node-linux-amd64')

    if ($CrossCheck) {
        # Portability must be verified, not assumed: only two OSes are deployed,
        # so nothing else forces the discipline.
        foreach ($t in 'linux/arm64', 'darwin/arm64') {
            $p = $t.Split('/')
            $env:GOOS = $p[0]; $env:GOARCH = $p[1]
            try { Invoke-Native { go vet ./... } "vet $t" } finally { $env:GOOS = ''; $env:GOARCH = '' }
            Ok "vets clean on $t"
        }
    }
} finally { Pop-Location }

# --- client -----------------------------------------------------------------
if (-not $SkipWeb) {
    Step 'Web client'
    Push-Location (Join-Path $Root 'web')
    try {
        if (-not (Test-Path node_modules)) { Invoke-Native { pnpm install } 'pnpm install' }
        Invoke-Native { pnpm check } 'svelte-check'
        Invoke-Native { pnpm build } 'pnpm build'
        $size = (Get-ChildItem build -Recurse -File | Measure-Object Length -Sum).Sum
        Ok ("web\build ({0:N1} KB)" -f ($size / 1KB))
    } finally { Pop-Location }
}

# --- android ----------------------------------------------------------------
if ($Android) {
    Step 'Android'
    Push-Location (Join-Path $Root 'android')
    try {
        # AGP does not support the system JDK 23; Studio ships a supported one.
        $jbr = 'C:\Program Files\Android\Android Studio\jbr'
        if (-not (Test-Path $jbr)) { throw "Android Studio JBR not found at $jbr" }
        $env:JAVA_HOME = $jbr

        # Release: R8-minified (~3 MB rather than ~56 MB for debug) and signed
        # with the debug key, so it still installs over earlier builds.
        Invoke-Native { & '.\gradlew.bat' assembleRelease --console=plain } 'gradle assembleRelease'

        $apk = Get-ChildItem 'app\build\outputs\apk\release' -Filter *.apk | Select-Object -First 1
        if (-not $apk) { throw 'no APK produced' }
        Copy-Item $apk.FullName (Join-Path $Dist 'remoter.apk') -Force
        Ok ("remoter.apk ({0:N1} MB)" -f ($apk.Length / 1MB))
    } finally { Pop-Location }
}

if ($Deploy) {
    & (Join-Path $PSScriptRoot 'deploy-hub.ps1') -SkipWeb:$SkipWeb
} else {
    Write-Host "`nNext: .\deploy\deploy-hub.ps1   (publish to the hub)" -ForegroundColor White
    Write-Host '      .\deploy\install-node.ps1  (make this PC controllable; elevated)' -ForegroundColor White
}
