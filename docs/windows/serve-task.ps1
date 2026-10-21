<#
.SYNOPSIS
Runs whatsapp-mcp for the "WhatsApp MCP" scheduled task and appends its log
output to a file.

.DESCRIPTION
register-task.ps1 points the scheduled task at this script; it is not meant to
be started by hand. The daemon logs to stderr, which Task Scheduler would
otherwise throw away, so this script copies stderr into -LogPath and exits
with the daemon's exit code, which lets the task's restart-on-failure setting
see a crash.

Each start appends a "--- start" line, each end a "--- exit code" line. A log
larger than 10 MB is moved to "<LogPath>.1" (replacing an older one) before
the daemon starts.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)] [string] $ExePath,
    [Parameter(Mandatory = $true)] [string] $Arguments,
    [Parameter(Mandatory = $true)] [string] $LogPath
)

$ErrorActionPreference = 'Stop'

$logDir = Split-Path -Parent $LogPath
if ($logDir -and -not (Test-Path -LiteralPath $logDir)) {
    New-Item -ItemType Directory -Force -Path $logDir | Out-Null
}
if ((Test-Path -LiteralPath $LogPath) -and (Get-Item -LiteralPath $LogPath).Length -gt 10MB) {
    Move-Item -LiteralPath $LogPath -Destination "$LogPath.1" -Force
}

# Buffer size 1 writes every chunk straight through, so the file is current
# while the daemon runs; FileShare ReadWrite lets you read it meanwhile.
$log = New-Object System.IO.FileStream($LogPath, [System.IO.FileMode]::Append,
    [System.IO.FileAccess]::Write, [System.IO.FileShare]::ReadWrite, 1)

function Write-LogLine([string] $text) {
    $line = '--- {0:yyyy-MM-ddTHH:mm:ssZ} {1}' -f [DateTime]::UtcNow, $text
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($line + "`r`n")
    $log.Write($bytes, 0, $bytes.Length)
}

$code = 1
try {
    Write-LogLine ('start: "{0}" {1}' -f $ExePath, $Arguments)

    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $ExePath
    $psi.Arguments = $Arguments
    $psi.WorkingDirectory = (Get-Location).ProviderPath
    $psi.UseShellExecute = $false
    $psi.RedirectStandardError = $true

    $proc = [System.Diagnostics.Process]::Start($psi)
    $copy = $proc.StandardError.BaseStream.CopyToAsync($log)
    $proc.WaitForExit()
    $copy.Wait()
    $code = $proc.ExitCode
    Write-LogLine "exit code $code"
}
catch {
    Write-LogLine "runner error: $($_.Exception.Message)"
}
finally {
    $log.Dispose()
}
exit $code
