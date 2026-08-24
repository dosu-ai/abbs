$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Fail-AbbsInstall([string]$Message) {
    throw "abbs installer: $Message"
}

if (-not [System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform(
    [System.Runtime.InteropServices.OSPlatform]::Windows
)) {
    Fail-AbbsInstall "install.ps1 supports Windows; use install.sh on macOS or Linux"
}

$architecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
switch ($architecture) {
    "X64" { $abbsArch = "amd64" }
    "Arm64" { $abbsArch = "arm64" }
    default { Fail-AbbsInstall "unsupported architecture: $architecture" }
}

$downloadBase = if ($env:ABBS_DOWNLOAD_BASE) {
    $env:ABBS_DOWNLOAD_BASE.TrimEnd("/")
} else {
    "https://github.com/dosu-ai/abbs/releases"
}

try {
    $downloadBaseUri = [Uri]$downloadBase
} catch {
    Fail-AbbsInstall "ABBS_DOWNLOAD_BASE is not a valid URL"
}
if ($downloadBaseUri.Scheme -ne "https" -and
    -not ($downloadBaseUri.Scheme -eq "http" -and $downloadBaseUri.IsLoopback)) {
    Fail-AbbsInstall "ABBS_DOWNLOAD_BASE must use HTTPS (HTTP is allowed only for loopback tests)"
}

if ($env:ABBS_VERSION) {
    $abbsTag = $env:ABBS_VERSION
    if (-not $abbsTag.StartsWith("v")) {
        $abbsTag = "v$abbsTag"
    }
} else {
    try {
        $latestResponse = Invoke-WebRequest -Uri "$downloadBase/latest" -MaximumRedirection 10 -UseBasicParsing
        $baseResponseProperties = @($latestResponse.BaseResponse.PSObject.Properties.Name)
        $finalUri = if ($baseResponseProperties -contains "RequestMessage" -and
            $latestResponse.BaseResponse.RequestMessage) {
            $latestResponse.BaseResponse.RequestMessage.RequestUri
        } elseif ($baseResponseProperties -contains "ResponseUri") {
            $latestResponse.BaseResponse.ResponseUri
        } else {
            Fail-AbbsInstall "could not determine the latest release redirect"
        }
        $abbsTag = $finalUri.Segments[-1].Trim("/")
    } catch {
        Fail-AbbsInstall "could not resolve the latest release: $($_.Exception.Message)"
    }
}

$abbsVersion = $abbsTag.TrimStart("v")
if ($abbsVersion -notmatch '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$') {
    Fail-AbbsInstall "invalid release version: $abbsTag"
}

$assetName = "abbs_${abbsVersion}_windows_${abbsArch}.zip"
$releaseUrl = "$downloadBase/download/$abbsTag"
$temporaryDir = Join-Path ([IO.Path]::GetTempPath()) ("abbs-install-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $temporaryDir | Out-Null
$installTemporary = $null

try {
    $archivePath = Join-Path $temporaryDir $assetName
    $checksumsPath = Join-Path $temporaryDir "checksums.txt"
    Write-Host "Downloading abbs $abbsVersion for windows/$abbsArch..."
    try {
        Invoke-WebRequest -Uri "$releaseUrl/$assetName" -OutFile $archivePath -UseBasicParsing
        Invoke-WebRequest -Uri "$releaseUrl/checksums.txt" -OutFile $checksumsPath -UseBasicParsing
    } catch {
        Fail-AbbsInstall "download failed: $($_.Exception.Message)"
    }

    $checksumPattern = '^([0-9A-Fa-f]{64})\s+\*?' + [Regex]::Escape($assetName) + '$'
    $checksumMatches = @(
        Get-Content $checksumsPath | Where-Object { $_ -match $checksumPattern }
    )
    if ($checksumMatches.Count -ne 1) {
        Fail-AbbsInstall "checksums.txt does not contain exactly one entry for $assetName"
    }
    $null = $checksumMatches[0] -match $checksumPattern
    $expectedHash = $Matches[1].ToLowerInvariant()
    $actualHash = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualHash -ne $expectedHash) {
        Fail-AbbsInstall "checksum verification failed"
    }

    $extractDir = Join-Path $temporaryDir "extracted"
    Expand-Archive -Path $archivePath -DestinationPath $extractDir
    $sourceBinary = Join-Path $extractDir "abbs.exe"
    if (-not (Test-Path -LiteralPath $sourceBinary -PathType Leaf)) {
        Fail-AbbsInstall "archive does not contain abbs.exe"
    }

    $installDir = if ($env:ABBS_INSTALL_DIR) {
        $env:ABBS_INSTALL_DIR
    } else {
        $localAppData = [Environment]::GetFolderPath([Environment+SpecialFolder]::LocalApplicationData)
        Join-Path $localAppData "Programs\abbs"
    }
    New-Item -ItemType Directory -Force -Path $installDir | Out-Null
    $targetBinary = Join-Path $installDir "abbs.exe"
    $installTemporary = Join-Path $installDir (".abbs.install." + [Guid]::NewGuid().ToString("N") + ".exe")
    [IO.File]::Copy($sourceBinary, $installTemporary, $true)
    if (Test-Path -LiteralPath $targetBinary) {
        [IO.File]::Replace($installTemporary, $targetBinary, $null, $true)
    } else {
        [IO.File]::Move($installTemporary, $targetBinary)
    }
    $installTemporary = $null

    Write-Host "Installed abbs $abbsVersion to $targetBinary"
    $pathEntries = @($env:Path -split ';')
    if (-not ($pathEntries | Where-Object { $_.TrimEnd('\') -ieq $installDir.TrimEnd('\') })) {
        Write-Host "Add $installDir to PATH, then run: abbs --version"
    }
} finally {
    if ($installTemporary -and (Test-Path -LiteralPath $installTemporary)) {
        Remove-Item -Force -LiteralPath $installTemporary
    }
    if (Test-Path -LiteralPath $temporaryDir) {
        Remove-Item -Recurse -Force -LiteralPath $temporaryDir
    }
}
