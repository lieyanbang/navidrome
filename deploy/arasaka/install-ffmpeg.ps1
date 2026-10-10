$ErrorActionPreference = 'Stop'

$version = '9.0.2'
$url = 'https://www.gyan.dev/ffmpeg/builds/packages/ffmpeg-9.0.2-essentials_build.zip'
$expectedSha256 = '60f467265b1e312373dbcd92200c2618a74850f98d3d078e94296bb3fa2047ba'
$workerRoot = Join-Path $env:ProgramData 'ArasakaFFmpegWorker'
$binaryDirectory = Join-Path $workerRoot 'ffmpeg\bin'
$ffmpegPath = Join-Path $binaryDirectory 'ffmpeg.exe'
if (Test-Path -LiteralPath $ffmpegPath) {
    $existingVersion = & $ffmpegPath -version 2>&1 | Select-Object -First 1
    if ($existingVersion -notmatch [regex]::Escape($version)) {
        throw "An unexpected FFmpeg executable already exists at $ffmpegPath"
    }
}

$staging = Join-Path $env:TEMP ('arasaka-ffmpeg-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $staging | Out-Null
try {
    $archive = Join-Path $staging 'ffmpeg.zip'
    Invoke-WebRequest -Uri $url -OutFile $archive
    $actualSha256 = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualSha256 -ne $expectedSha256) {
        throw "FFmpeg package SHA-256 mismatch: got $actualSha256"
    }
    Write-Output "Verified source package SHA-256 $actualSha256."
    $unpacked = Join-Path $staging 'unpacked'
    Expand-Archive -LiteralPath $archive -DestinationPath $unpacked
    $sourceDirectory = Get-ChildItem -LiteralPath $unpacked -Directory | Select-Object -First 1
    if (-not $sourceDirectory) {
        throw 'FFmpeg package did not contain its expected top-level directory.'
    }
    New-Item -ItemType Directory -Force -Path $binaryDirectory | Out-Null
    foreach ($name in @('ffmpeg.exe', 'ffprobe.exe')) {
        $source = Join-Path $sourceDirectory.FullName "bin\$name"
        if (-not (Test-Path -LiteralPath $source)) {
            throw "FFmpeg package is missing $name"
        }
        Copy-Item -LiteralPath $source -Destination $binaryDirectory -Force
    }
    foreach ($name in @('README.txt', 'LICENSE.txt')) {
        $source = Join-Path $sourceDirectory.FullName $name
        if (Test-Path -LiteralPath $source) {
            Copy-Item -LiteralPath $source -Destination $workerRoot -Force
        }
    }
    $versionOutput = & $ffmpegPath -version 2>&1 | Select-Object -First 1
    if ($versionOutput -notmatch [regex]::Escape($version)) {
        throw "Installed FFmpeg version did not match $version."
    }
    $encoderOutput = (& $ffmpegPath -hide_banner -encoders 2>&1 | Out-String)
    if ($encoderOutput -notmatch 'libmp3lame') {
        throw 'Installed FFmpeg does not expose libmp3lame.'
    }
    [ordered]@{
        version = $version
        source = $url
        sha256 = $actualSha256
        verified_at_utc = [DateTime]::UtcNow.ToString('o')
        encoder = 'libmp3lame'
    } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $workerRoot 'ffmpeg-provenance.json') -Encoding ascii
    Write-Output $versionOutput
    Write-Output "Verified FFmpeg package SHA-256 $actualSha256 and libmp3lame capability."
}
finally {
    $tempRoot = [IO.Path]::GetFullPath($env:TEMP).TrimEnd('\') + '\'
    $stagingPath = [IO.Path]::GetFullPath($staging)
    if ($stagingPath.StartsWith($tempRoot, [StringComparison]::OrdinalIgnoreCase) -and
        (Split-Path -Leaf $stagingPath).StartsWith('arasaka-ffmpeg-', [StringComparison]::Ordinal)) {
        Remove-Item -LiteralPath $stagingPath -Recurse -Force
    }
}
