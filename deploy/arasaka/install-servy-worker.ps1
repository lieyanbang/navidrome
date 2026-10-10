param(
    [string]$PythonExe = 'C:\Users\lieyanbang\AppData\Local\Programs\Python\Python314\python.exe',
    [string]$WorkerRoot = 'C:\ProgramData\ArasakaFFmpegWorker',
    [string]$SourceRoot = 'C:\Users\lieyanbang\worker-upload',
    [string]$KeyUpload = 'C:\Users\lieyanbang\worker-key-upload.txt',
    [string]$ServyCli = 'C:\Progra~1\Servy\servy-cli.exe'
)

$ErrorActionPreference = 'Stop'
$bind = '192.168.137.1'
$workerPort = 13324
$workerService = 'ArasakaFFmpegWorker'
$watchdogService = 'ArasakaFFmpegWatchdog'
$secretFile = Join-Path $WorkerRoot 'secret.key'
$stateDirectory = Join-Path $WorkerRoot 'state'
$logsDirectory = Join-Path $WorkerRoot 'logs'
$workerModule = Join-Path $WorkerRoot 'worker\arasaka_worker\server.py'
$watchdogModule = Join-Path $WorkerRoot 'worker\arasaka_worker\watchdog.py'
$sourceWorkerRoot = Join-Path $SourceRoot 'worker'
$ffmpeg = Join-Path $WorkerRoot 'ffmpeg\bin\ffmpeg.exe'

function Set-WorkerAcl {
    $workerDirectories = @((Get-Item -LiteralPath $WorkerRoot)) + @(Get-ChildItem -LiteralPath $WorkerRoot -Directory -Recurse -Force)
    foreach ($directory in $workerDirectories) {
        & icacls.exe $directory.FullName /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Failed to restrict worker directory $($directory.FullName)." }
    }
    $workerFiles = Get-ChildItem -LiteralPath $WorkerRoot -File -Recurse -Force
    foreach ($file in $workerFiles) {
        & icacls.exe $file.FullName /inheritance:r /grant:r '*S-1-5-18:F' '*S-1-5-32-544:F' | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Failed to restrict worker file $($file.FullName)." }
    }
}

if (-not (Test-Path -LiteralPath $ServyCli)) { throw "Servy CLI not found: $ServyCli" }
if (-not (Test-Path -LiteralPath $PythonExe)) { throw "Python 3.14 not found: $PythonExe" }
if (-not (Test-Path -LiteralPath (Join-Path $sourceWorkerRoot 'arasaka_worker\server.py')) -or
    -not (Test-Path -LiteralPath (Join-Path $sourceWorkerRoot 'arasaka_worker\watchdog.py'))) {
    throw "Worker source files are missing under $SourceRoot"
}
if (Test-Path -LiteralPath $WorkerRoot) { Set-WorkerAcl }
if (Test-Path -LiteralPath $KeyUpload) {
    $operatorSid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    & icacls.exe $KeyUpload /inheritance:r /grant:r "*$($operatorSid):(R)" '*S-1-5-18:(F)' '*S-1-5-32-544:(F)' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Failed to restrict the temporary HMAC key upload.' }
}
Copy-Item -LiteralPath $sourceWorkerRoot -Destination $WorkerRoot -Recurse -Force
if (-not (Test-Path -LiteralPath $workerModule) -or -not (Test-Path -LiteralPath $watchdogModule)) {
    throw "Worker source files were not installed under $WorkerRoot"
}
if (-not (Test-Path -LiteralPath $ffmpeg)) { throw "Verified FFmpeg is missing: $ffmpeg" }
if ((Test-Path -LiteralPath $KeyUpload) -and (Test-Path -LiteralPath $secretFile)) {
    if ((Get-Content -LiteralPath $KeyUpload -Raw).Trim() -cne (Get-Content -LiteralPath $secretFile -Raw).Trim()) {
        throw 'A different HMAC key is already installed; refusing to rotate it implicitly.'
    }
} elseif (Test-Path -LiteralPath $KeyUpload) {
    Copy-Item -LiteralPath $KeyUpload -Destination $secretFile
}
if (-not (Test-Path -LiteralPath $secretFile)) { throw "Shared HMAC key is missing: $secretFile" }
$keyText = (Get-Content -LiteralPath $secretFile -Raw).Trim()
if ($keyText -notmatch '\A[0-9a-fA-F]{64,}\z' -or ($keyText.Length % 2) -ne 0) {
    throw 'Worker HMAC key must be at least 32 bytes encoded as hexadecimal.'
}
if (-not (Get-NetIPAddress -AddressFamily IPv4 -IPAddress $bind -ErrorAction SilentlyContinue)) {
    throw "The expected worker bind address $bind is not assigned to this host."
}
if (Get-NetTCPConnection -State Listen -LocalPort $workerPort -ErrorAction SilentlyContinue) {
    throw "TCP port $workerPort already has a listener. No service or rule was changed."
}
$pythonVersion = (& $PythonExe --version 2>&1 | Out-String).Trim()
if ($pythonVersion -notmatch '^Python 3\.14\.') { throw "Expected Python 3.14, found $pythonVersion" }
$encoderOutput = & $ffmpeg -hide_banner -encoders 2>&1 | Out-String
if ($encoderOutput -notmatch 'libmp3lame') { throw 'FFmpeg does not expose libmp3lame.' }

New-Item -ItemType Directory -Force -Path $stateDirectory, $logsDirectory | Out-Null
Set-WorkerAcl

$firewallRuleName = 'ArasakaFFmpegWorker-OneCloud-13324'
$firewallRule = Get-NetFirewallRule -Name $firewallRuleName -PolicyStore ActiveStore -ErrorAction SilentlyContinue
if ($firewallRule) {
    $portFilter = $firewallRule | Get-NetFirewallPortFilter
    $addressFilter = $firewallRule | Get-NetFirewallAddressFilter
    $appFilter = $firewallRule | Get-NetFirewallApplicationFilter
    if ($firewallRule.Enabled -ne 'True' -or $firewallRule.Direction -ne 'Inbound' -or
        $firewallRule.Action -ne 'Allow' -or $portFilter.Protocol -ne 'TCP' -or
        ($portFilter.LocalPort -join ',') -ne "$workerPort" -or
        ($addressFilter.LocalAddress -join ',') -ne $bind -or
        ($addressFilter.RemoteAddress -join ',') -ne '192.168.137.11' -or
        $appFilter.Program -ine $PythonExe) {
        throw "Firewall rule $firewallRuleName already exists with unexpected settings."
    }
} else {
    $existingProgramFilters = @(
        Get-NetFirewallRule -PolicyStore ActiveStore |
            Where-Object { $_.Name -ne $firewallRuleName } |
            ForEach-Object { $_ | Get-NetFirewallApplicationFilter } |
            Where-Object { $_.Program -ieq $PythonExe }
    )
    if ($existingProgramFilters.Count -gt 0) {
        throw 'A pre-existing firewall application filter targets this Python executable; inspect it before binding the worker.'
    }
    New-NetFirewallRule -Name $firewallRuleName -DisplayName 'Arasaka FFmpeg Worker from OneCloud' `
        -Direction Inbound -Action Allow -Enabled True -Profile Any -Protocol TCP `
        -LocalAddress $bind -LocalPort $workerPort -RemoteAddress '192.168.137.11' `
        -Program $PythonExe | Out-Null
}

foreach ($name in @($workerService, $watchdogService)) {
    $existing = Get-CimInstance Win32_Service -Filter "Name='$name'" -ErrorAction SilentlyContinue
    if ($existing -and $existing.PathName -notmatch 'Servy\.Host\.exe') {
        throw "A non-Servy Windows service named $name already exists."
    }
}

$workerEnvironment = @(
    "ARASAKA_WORKER_BIND=$bind",
    "ARASAKA_WORKER_PORT=$workerPort",
    "ARASAKA_WORKER_FFMPEG=$ffmpeg",
    "ARASAKA_WORKER_SECRET_FILE=$secretFile",
    "ARASAKA_WORKER_REPLAY_DB=$(Join-Path $stateDirectory 'replay.sqlite3')",
    'ARASAKA_WORKER_MAX_INPUT_BYTES=2147483648',
    'ARASAKA_WORKER_MAX_DURATION_SECONDS=21600'
) -join ';'
$watchdogEnvironment = @(
    "ARASAKA_WATCHDOG_URL=http://$bind`:$workerPort",
    "ARASAKA_SERVY_CLI=$ServyCli",
    "ARASAKA_WORKER_SERVICE_NAME=$workerService",
    'ARASAKA_WATCHDOG_INTERVAL_SECONDS=10',
    'ARASAKA_WATCHDOG_MAX_FAILURES=3',
    'ARASAKA_WATCHDOG_RESTART_COOLDOWN_SECONDS=120'
) -join ';'

if (-not (Get-CimInstance Win32_Service -Filter "Name='$workerService'" -ErrorAction SilentlyContinue)) {
    $workerArgs = @(
        'install', "--name=$workerService", '--displayName=Arasaka FFmpeg Worker',
        '--description=Authenticated fixed-profile Navidrome media worker on the LAN',
        "--path=$PythonExe", "--startupDir=$WorkerRoot", '--params=-m worker.arasaka_worker.server',
        '--startupType=Automatic', '--priority=BelowNormal',
        "--stdout=$(Join-Path $logsDirectory 'worker.stdout.log')",
        "--stderr=$(Join-Path $logsDirectory 'worker.stderr.log')",
        '--enableSizeRotation', '--rotationSize=10', '--maxRotations=5',
        '--enableHealth', '--heartbeatInterval=10', '--maxFailedChecks=3',
        '--recoveryAction=RestartProcess', '--maxRestartAttempts=10', "--envVars=$workerEnvironment"
    )
    & $ServyCli @workerArgs
    if ($LASTEXITCODE -ne 0) { throw 'Servy failed to register ArasakaFFmpegWorker.' }
}

if (-not (Get-CimInstance Win32_Service -Filter "Name='$watchdogService'" -ErrorAction SilentlyContinue)) {
    $watchdogArgs = @(
        'install', "--name=$watchdogService", '--displayName=Arasaka FFmpeg Watchdog',
        '--description=HTTP health and readiness watchdog for ArasakaFFmpegWorker',
        "--path=$PythonExe", "--startupDir=$WorkerRoot", '--params=-m worker.arasaka_worker.watchdog',
        '--startupType=Automatic', '--priority=BelowNormal',
        "--stdout=$(Join-Path $logsDirectory 'watchdog.stdout.log')",
        "--stderr=$(Join-Path $logsDirectory 'watchdog.stderr.log')",
        '--enableSizeRotation', '--rotationSize=10', '--maxRotations=5',
        '--enableHealth', '--heartbeatInterval=10', '--maxFailedChecks=3',
        '--recoveryAction=RestartProcess', '--maxRestartAttempts=10', "--envVars=$watchdogEnvironment"
    )
    & $ServyCli @watchdogArgs
    if ($LASTEXITCODE -ne 0) { throw 'Servy failed to register ArasakaFFmpegWatchdog.' }
}

foreach ($name in @($workerService, $watchdogService)) {
    $service = Get-Service -Name $name -ErrorAction Stop
    if ($service.Status -eq 'Running') {
        & $ServyCli restart --quiet "--name=$name"
        if ($LASTEXITCODE -ne 0) { throw "Servy could not restart $name." }
    } else {
        & $ServyCli start --quiet "--name=$name"
        if ($LASTEXITCODE -ne 0) { throw "Servy could not start $name." }
    }
}

Write-Output "Servy worker and watchdog registered. Firewall scope is source 192.168.137.11 to $bind`:$workerPort."
& $ServyCli show
$expectedKeyUpload = Join-Path $env:USERPROFILE 'worker-key-upload.txt'
if ((Test-Path -LiteralPath $KeyUpload) -and
    [IO.Path]::GetFullPath($KeyUpload).Equals([IO.Path]::GetFullPath($expectedKeyUpload), [StringComparison]::OrdinalIgnoreCase)) {
    Remove-Item -LiteralPath $KeyUpload -Force
}
