[CmdletBinding()]
param(
    [string]$GoExe = (Join-Path $env:LOCALAPPDATA 'Programs\Go\1.27.2\bin\go.exe'),
    [string]$OutputDirectory = (Join-Path $PSScriptRoot '..\..\outputs\plugin-metadata-20261013\artifacts')
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.IO.Compression
Add-Type -AssemblyName System.IO.Compression.FileSystem

$goPath = (Resolve-Path -LiteralPath $GoExe).Path
$goVersion = (& $goPath version | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $goVersion -notmatch '\bgo1\.27\.2\b') {
    throw "This build is pinned to Go 1.27.2; found '$goVersion'."
}

$outputPath = [IO.Path]::GetFullPath($OutputDirectory)
[IO.Directory]::CreateDirectory($outputPath) | Out-Null
$tempRoot = Join-Path ([IO.Path]::GetTempPath()) ("navidrome-plugin-build-" + [Guid]::NewGuid().ToString('N'))
[IO.Directory]::CreateDirectory($tempRoot) | Out-Null

$oldGoos = $env:GOOS
$oldGoarch = $env:GOARCH
$oldCgo = $env:CGO_ENABLED
$oldGoFlags = $env:GOFLAGS

function New-DeterministicPackage {
    param(
        [Parameter(Mandatory)][string]$ManifestPath,
        [Parameter(Mandatory)][string]$WasmPath,
        [Parameter(Mandatory)][string]$PackagePath
    )

    $stream = [IO.File]::Open($PackagePath, [IO.FileMode]::CreateNew, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
    $archive = [System.IO.Compression.ZipArchive]::new($stream, [System.IO.Compression.ZipArchiveMode]::Create, $true)
    try {
        foreach ($item in @(
            @{ Name = 'manifest.json'; Path = $ManifestPath },
            @{ Name = 'plugin.wasm'; Path = $WasmPath }
        )) {
            $entry = $archive.CreateEntry($item.Name, [System.IO.Compression.CompressionLevel]::NoCompression)
            $entry.LastWriteTime = [DateTimeOffset]::new(1980, 1, 1, 0, 0, 0, [TimeSpan]::Zero)
            $entry.ExternalAttributes = 0
            $input = [IO.File]::OpenRead($item.Path)
            $destination = $entry.Open()
            try {
                $input.CopyTo($destination)
            } finally {
                $destination.Dispose()
                $input.Dispose()
            }
        }
    } finally {
        $archive.Dispose()
        $stream.Dispose()
    }
}

try {
    $env:GOOS = 'wasip1'
    $env:GOARCH = 'wasm'
    $env:CGO_ENABLED = '0'
    $env:GOFLAGS = ''

    $results = @()
    foreach ($pluginID in @('coverartarchive', 'wikimedia')) {
        $pluginDir = Join-Path $PSScriptRoot $pluginID
        $manifestPath = Join-Path $pluginDir 'manifest.json'
        $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
        if ($manifest.name -ne $pluginID) {
            throw "Plugin directory '$pluginID' has manifest name '$($manifest.name)'."
        }

        $packages = @()
        for ($pass = 1; $pass -le 2; $pass++) {
            $passDir = Join-Path $tempRoot "$pluginID-$pass"
            [IO.Directory]::CreateDirectory($passDir) | Out-Null
            $wasmPath = Join-Path $passDir 'plugin.wasm'
            $packagePath = Join-Path $passDir "$pluginID.ndp"
            Push-Location $pluginDir
            try {
                & $goPath build -mod=readonly -trimpath -buildvcs=false -ldflags=-buildid= -buildmode=c-shared -o $wasmPath .
                if ($LASTEXITCODE -ne 0) {
                    throw "Go WASM build failed for '$pluginID' (pass $pass) with exit code $LASTEXITCODE."
                }
            } finally {
                Pop-Location
            }
            New-DeterministicPackage -ManifestPath $manifestPath -WasmPath $wasmPath -PackagePath $packagePath
            $packages += $packagePath
        }

        $firstHash = (Get-FileHash -LiteralPath $packages[0] -Algorithm SHA256).Hash
        $secondHash = (Get-FileHash -LiteralPath $packages[1] -Algorithm SHA256).Hash
        if ($firstHash -ne $secondHash) {
            throw "Non-reproducible package for '$pluginID': $firstHash differs from $secondHash."
        }

        $targetPath = Join-Path $outputPath "$pluginID.ndp"
        Copy-Item -LiteralPath $packages[0] -Destination $targetPath -Force
        $package = [System.IO.Compression.ZipFile]::OpenRead($targetPath)
        try {
            $names = @($package.Entries | ForEach-Object FullName | Sort-Object)
            if ($names.Count -ne 2 -or $names[0] -ne 'manifest.json' -or $names[1] -ne 'plugin.wasm') {
                throw "Package '$targetPath' contains unexpected entries: $($names -join ', ')."
            }
        } finally {
            $package.Dispose()
        }

        $file = Get-Item -LiteralPath $targetPath
        $results += [pscustomobject]@{
            id = $pluginID
            path = $targetPath
            size_bytes = $file.Length
            sha256 = (Get-FileHash -LiteralPath $targetPath -Algorithm SHA256).Hash.ToLowerInvariant()
            reproducible_two_builds = $true
            zip_entries = $names
        }
    }

    $results | ConvertTo-Json -Depth 4
} finally {
    $env:GOOS = $oldGoos
    $env:GOARCH = $oldGoarch
    $env:CGO_ENABLED = $oldCgo
    $env:GOFLAGS = $oldGoFlags
    if (Test-Path -LiteralPath $tempRoot) {
        Remove-Item -LiteralPath $tempRoot -Recurse -Force
    }
}
