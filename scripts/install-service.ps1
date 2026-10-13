<#
.SYNOPSIS
Installs the fulcrum dashboard as a Windows service, using NSSM.

.DESCRIPTION
Creates (or updates) a service named FulcrumDashboard that runs
"fulcrum.exe -serve" under the LOCAL SERVICE account and starts with Windows.

The service's working directory is the directory of the executable. What the
program prints is logged there, in FulcrumDashboard.out.log (standard output)
and FulcrumDashboard.err.log (standard error).

LOCAL SERVICE cannot normally read files in a user's profile, so the script
also grants that account the access the service needs: read on the executable
directory, the settings file and the data directory, and write on the two
log files' directory.

Run it from an elevated ("Run as administrator") PowerShell. NSSM must be
installed: https://nssm.cc, or "choco install nssm" / "winget install nssm".

.PARAMETER ExePath
The fulcrum executable. Default: bin\fulcrum.exe in the repository.

.PARAMETER EnvFile
The .env file with the settings. Default: .env in the repository.

.PARAMETER DataDir
The directory holding the downloaded share. Default: data in the repository.

.PARAMETER Addr
The address the dashboard listens on. Default: localhost:8080, which only
accepts connections from this computer. Use ":8080" to serve the network.

.PARAMETER NssmPath
The nssm executable. Default: nssm on the PATH.

.PARAMETER NoStart
Install the service without starting it.

.PARAMETER Remove
Stop and remove the service instead of installing it. The access granted to
LOCAL SERVICE and the log files are left in place.

.EXAMPLE
scripts\install-service.ps1

.EXAMPLE
scripts\install-service.ps1 -Addr :8080 -DataDir D:\fulcrum\data

.EXAMPLE
scripts\install-service.ps1 -Remove
#>
[CmdletBinding()]
param(
    [string]$ExePath,
    [string]$EnvFile,
    [string]$DataDir,
    [string]$Addr = 'localhost:8080',
    [string]$NssmPath = 'nssm',
    [switch]$NoStart,
    [switch]$Remove
)

$ErrorActionPreference = 'Stop'

$ServiceName = 'FulcrumDashboard'
# The account as the service manager names it, and its well-known SID, which
# icacls accepts on every language version of Windows.
$ServiceAccount = 'NT AUTHORITY\LocalService'
$ServiceAccountSid = '*S-1-5-19'

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$isAdmin = ([Security.Principal.WindowsPrincipal]$identity).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    throw 'This script must be run from an elevated (Run as administrator) PowerShell.'
}

$nssm = Get-Command $NssmPath -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
if (-not $nssm) {
    throw "NSSM was not found ($NssmPath). Install it from https://nssm.cc or pass -NssmPath."
}
$nssmExe = $nssm.Source

# Nssm runs nssm with the given arguments and fails if it reports an error.
function Nssm {
    & $nssmExe @args
    if ($LASTEXITCODE -ne 0) { throw "nssm $($args -join ' ') failed (exit code $LASTEXITCODE)" }
}

# Grant gives LOCAL SERVICE the given icacls rights on a file or directory.
function Grant([string]$Path, [string]$Rights) {
    icacls $Path /grant "${ServiceAccountSid}:$Rights" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "could not grant LOCAL SERVICE access to $Path" }
}

$exists = [bool](Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)

if ($Remove) {
    if (-not $exists) {
        Write-Host "Service $ServiceName is not installed."
        return
    }
    & $nssmExe stop $ServiceName | Out-Null
    Nssm remove $ServiceName confirm
    Write-Host "Service $ServiceName removed."
    return
}

$repo = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
if (-not $ExePath) { $ExePath = Join-Path $repo 'bin\fulcrum.exe' }
if (-not $EnvFile) { $EnvFile = Join-Path $repo '.env' }
if (-not $DataDir) { $DataDir = Join-Path $repo 'data' }

if (-not (Test-Path $ExePath -PathType Leaf)) {
    throw "$ExePath not found. Build it first with scripts\build.ps1, or pass -ExePath."
}
if (-not (Test-Path $EnvFile -PathType Leaf)) { throw "$EnvFile not found. Pass -EnvFile." }
if (-not (Test-Path $DataDir -PathType Container)) {
    throw "$DataDir not found. Run fulcrum once to download the share, or pass -DataDir."
}
$ExePath = (Resolve-Path $ExePath).Path
$EnvFile = (Resolve-Path $EnvFile).Path
$DataDir = (Resolve-Path $DataDir).Path
$exeDir = Split-Path $ExePath -Parent
$stdoutLog = Join-Path $exeDir "$ServiceName.out.log"
$stderrLog = Join-Path $exeDir "$ServiceName.err.log"

# The settings and data are given as full paths, because the service's
# working directory is the executable's, not the repository's.
$arguments = "-serve -addr `"$Addr`" -env `"$EnvFile`" -out `"$DataDir`""

if ($exists) {
    Write-Host "Updating service $ServiceName..."
    & $nssmExe stop $ServiceName | Out-Null
    Nssm set $ServiceName Application $ExePath
}
else {
    Write-Host "Installing service $ServiceName..."
    Nssm install $ServiceName $ExePath
}

Nssm set $ServiceName AppParameters $arguments
Nssm set $ServiceName AppDirectory $exeDir
Nssm set $ServiceName AppStdout $stdoutLog
Nssm set $ServiceName AppStderr $stderrLog
# Start a new log file once the current one reaches 10 MB.
Nssm set $ServiceName AppRotateFiles 1
Nssm set $ServiceName AppRotateOnline 1
Nssm set $ServiceName AppRotateBytes 10485760
Nssm set $ServiceName DisplayName 'Fulcrum Dashboard'
Nssm set $ServiceName Description "Serves the Fulcrum inspection dashboard on http://$Addr"
Nssm set $ServiceName Start SERVICE_AUTO_START
# LOCAL SERVICE has no password; NSSM still expects one, so it is empty.
Nssm set $ServiceName ObjectName $ServiceAccount ''

# Read and execute on the program and NSSM, modify on the executable's
# directory for the logs, read on the settings and the data.
Grant $exeDir '(OI)(CI)M'
Grant $nssmExe 'RX'
Grant $EnvFile 'R'
Grant $DataDir '(OI)(CI)RX'

$shown = if ($Addr.StartsWith(':')) { "localhost$Addr" } else { $Addr }
if ($NoStart) {
    Write-Host "Service $ServiceName installed. Start it with: nssm start $ServiceName"
}
else {
    Nssm start $ServiceName
    Start-Sleep -Seconds 2
    $status = (Get-Service -Name $ServiceName).Status
    Write-Host "Service $ServiceName is $status."
    if ($status -ne 'Running') {
        Write-Warning "The service did not stay running. See $stderrLog"
    }
}
Write-Host "Dashboard: http://$shown"
Write-Host "Logs:      $stdoutLog"
Write-Host "           $stderrLog"
