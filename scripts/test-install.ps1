param(
    [Parameter(Mandatory = $true)]
    [string]$DistDir
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$repoDir = Split-Path -Parent $PSScriptRoot
$installer = Join-Path $repoDir "install.ps1"
$null = [scriptblock]::Create((Get-Content -Raw -LiteralPath $installer))

$archive = @(Get-ChildItem -Path $DistDir -Filter "abbs_*_windows_amd64.zip")
if ($archive.Count -ne 1) {
    throw "could not select one Windows amd64 archive"
}
if ($archive[0].Name -notmatch '^abbs_(.+)_windows_amd64\.zip$') {
    throw "unexpected archive name: $($archive[0].Name)"
}
$version = $Matches[1]
$tag = "v$version"
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ("abbs-installer-test-" + [Guid]::NewGuid().ToString("N"))
$fixtureRoot = Join-Path $testRoot "fixtures"
$releaseDir = Join-Path $fixtureRoot "releases\download\$tag"
New-Item -ItemType Directory -Path $releaseDir | Out-Null
Copy-Item -LiteralPath $archive[0].FullName -Destination (Join-Path $releaseDir $archive[0].Name)
Copy-Item -LiteralPath (Join-Path $DistDir "checksums.txt") -Destination (Join-Path $releaseDir "checksums.txt")

$addressFile = Join-Path $testRoot "address"
$serverScript = Join-Path $repoDir "scripts\release-fixture-server.py"
$serverProcess = Start-Process -FilePath "python" -ArgumentList @(
    "`"$serverScript`"",
    "`"$fixtureRoot`"",
    $tag,
    "`"$addressFile`""
) -PassThru -NoNewWindow

try {
    for ($attempt = 0; $attempt -lt 100 -and -not (Test-Path -LiteralPath $addressFile); $attempt++) {
        Start-Sleep -Milliseconds 50
    }
    if (-not (Test-Path -LiteralPath $addressFile)) {
        throw "fixture server did not start"
    }
    $downloadBase = (Get-Content -Raw -LiteralPath $addressFile).Trim() + "/releases"

    $explicitDir = Join-Path $testRoot "explicit"
    $env:ABBS_VERSION = $tag
    $env:ABBS_INSTALL_DIR = $explicitDir
    $env:ABBS_DOWNLOAD_BASE = $downloadBase
    & $installer
    $reportedVersion = & (Join-Path $explicitDir "abbs.exe") --version
    if ($reportedVersion -ne "abbs $version") {
        throw "installed binary reported $reportedVersion, want abbs $version"
    }

    Add-Content -NoNewline -LiteralPath (Join-Path $releaseDir $archive[0].Name) -Value "tamper"
    $env:ABBS_INSTALL_DIR = Join-Path $testRoot "tampered"
    $tamperRejected = $false
    try {
        & $installer
    } catch {
        $tamperRejected = $_.Exception.Message -match 'checksum verification failed'
    }
    if (-not $tamperRejected) {
        throw "installer did not reject a checksum mismatch"
    }
    if (Test-Path -LiteralPath (Join-Path $env:ABBS_INSTALL_DIR "abbs.exe")) {
        throw "tampered binary was installed"
    }

    $env:ABBS_VERSION = "not-semver"
    $invalidRejected = $false
    try {
        & $installer
    } catch {
        $invalidRejected = $_.Exception.Message -match 'invalid release version'
    }
    if (-not $invalidRejected) {
        throw "installer accepted an invalid version"
    }

    Write-Host "verified install.ps1 syntax, install, version output, tamper rejection, and error paths"
} finally {
    foreach ($environmentName in @("ABBS_VERSION", "ABBS_INSTALL_DIR", "ABBS_DOWNLOAD_BASE")) {
        Remove-Item "Env:$environmentName" -ErrorAction SilentlyContinue
    }
    if ($serverProcess -and -not $serverProcess.HasExited) {
        Stop-Process -Id $serverProcess.Id -Force
    }
    if (Test-Path -LiteralPath $testRoot) {
        Remove-Item -Recurse -Force -LiteralPath $testRoot
    }
}
