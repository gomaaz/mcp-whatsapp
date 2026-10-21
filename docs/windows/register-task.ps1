<#
.SYNOPSIS
Registers or removes the per-user scheduled task "WhatsApp MCP", which starts
whatsapp-mcp in read-only mode when you log on.

.DESCRIPTION
The task runs as the current user, only while that user is logged on, so no
password is stored. It starts

    <ExePath> -read-only -store <StorePath> serve <ExtraArgs>

in the repository folder, without a visible window, and appends the daemon's
log to -LogPath (through serve-task.ps1 next to this script). If the daemon
exits with an error, Task Scheduler restarts the task up to 3 times, one
minute apart. There is no time limit.

Running the script again replaces the existing task with the new settings.
The script does not start the task; see the hint it prints at the end.

.PARAMETER ExePath
The whatsapp-mcp binary. Default: <repo>\bin\whatsapp-mcp.exe.

.PARAMETER StorePath
The store directory with messages.db and whatsapp.db. Default: <repo>\store.

.PARAMETER ExtraArgs
More serve flags, appended after "serve" as written, e.g.
"-media-retention 30d" or "-addr 127.0.0.1:8766". Default: none.

.PARAMETER LogPath
Log file. Default: %LOCALAPPDATA%\whatsapp-mcp\serve.log.

.PARAMETER Unregister
Stop and remove the task instead of registering it.

.EXAMPLE
.\docs\windows\register-task.ps1 -StorePath C:\Users\me\whatsapp-store

.EXAMPLE
.\docs\windows\register-task.ps1 -Unregister
#>
[CmdletBinding()]
param(
    [string] $ExePath,
    [string] $StorePath,
    [string] $ExtraArgs = '',
    [string] $LogPath,
    [switch] $Unregister
)

$ErrorActionPreference = 'Stop'
$TaskName = 'WhatsApp MCP'
$RepoRoot = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..\..')).ProviderPath

function Get-FullPath([string] $path) {
    $full = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($path)
    # A trailing backslash inside quotes would escape the closing quote.
    if ($full.Length -gt 3) { $full = $full.TrimEnd('\') }
    if ($full.Contains('"')) { throw "Path must not contain a double quote: $full" }
    return $full
}

# Quotes one argument for a Windows command line (Go and PowerShell both
# split their command line by the usual CommandLineToArgvW rules).
function Format-Arg([string] $value) {
    if ($value -match '[\s]') { return '"' + $value + '"' }
    return $value
}

# Single-quoted PowerShell literal.
function Format-Literal([string] $value) {
    return "'" + ($value -replace "'", "''") + "'"
}

if (-not $ExePath) { $ExePath = Join-Path $RepoRoot 'bin\whatsapp-mcp.exe' }
if (-not $StorePath) { $StorePath = Join-Path $RepoRoot 'store' }
if (-not $LogPath) { $LogPath = Join-Path $env:LOCALAPPDATA 'whatsapp-mcp\serve.log' }
$ExePath = Get-FullPath $ExePath
$StorePath = Get-FullPath $StorePath
$LogPath = Get-FullPath $LogPath

if ($Unregister) {
    $task = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    if (-not $task) {
        Write-Host "No scheduled task named '$TaskName' is registered."
    }
    else {
        Stop-ScheduledTask -TaskName $TaskName
        Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
        Write-Host "Removed scheduled task '$TaskName'."
    }
    $left = @(Get-Process -Name 'whatsapp-mcp' -ErrorAction SilentlyContinue |
        Where-Object { $_.Path -eq $ExePath })
    if ($left.Count -gt 0) {
        $ids = ($left | ForEach-Object { $_.Id }) -join ', '
        Write-Warning "whatsapp-mcp.exe from $ExePath is still running (PID $ids). It keeps the store locked. Stop it with: Stop-Process -Id $ids"
    }
    return
}

if (-not (Test-Path -LiteralPath $ExePath -PathType Leaf)) {
    throw "Binary not found: $ExePath. Build it first (make build), or pass -ExePath."
}
if (-not (Test-Path -LiteralPath $StorePath -PathType Container)) {
    Write-Warning "Store directory $StorePath does not exist yet. serve creates it; pair at http://127.0.0.1:8765/pair once the task runs."
}

$runner = Join-Path $PSScriptRoot 'serve-task.ps1'
if (-not (Test-Path -LiteralPath $runner -PathType Leaf)) {
    throw "Runner script not found: $runner"
}

$exeArgs = '-read-only -store {0} serve' -f (Format-Arg $StorePath)
if ($ExtraArgs.Trim()) { $exeArgs += ' ' + $ExtraArgs.Trim() }

# The task starts PowerShell with an encoded command, so paths and ExtraArgs
# need no quoting on the task's own command line. conhost --headless keeps the
# console invisible, also when Windows Terminal is the default terminal.
$command = '& {0} -ExePath {1} -Arguments {2} -LogPath {3}; exit $LASTEXITCODE' -f `
    (Format-Literal $runner), (Format-Literal $ExePath), (Format-Literal $exeArgs), (Format-Literal $LogPath)
$encoded = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($command))
$powershell = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
$conhost = Join-Path $env:SystemRoot 'System32\conhost.exe'

$action = New-ScheduledTaskAction -Execute $conhost -WorkingDirectory $RepoRoot `
    -Argument ('--headless {0} -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand {1}' -f (Format-Arg $powershell), $encoded)

$userId = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $userId
# Interactive: runs only while the user is logged on, no stored password.
$principal = New-ScheduledTaskPrincipal -UserId $userId -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet `
    -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -MultipleInstances IgnoreNew `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries

Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
    -Principal $principal -Settings $settings -Force `
    -Description 'whatsapp-mcp serve in read-only mode (registered by docs/windows/register-task.ps1)' | Out-Null

Write-Host "Registered scheduled task '$TaskName' for $userId."
Write-Host "  Command: `"$ExePath`" $exeArgs"
Write-Host "  Folder:  $RepoRoot"
Write-Host "  Log:     $LogPath"
Write-Host "It starts at your next logon. To start it now (stop any whatsapp-mcp you started by hand first):"
Write-Host "  Start-ScheduledTask -TaskName '$TaskName'"
