# Start whatsapp-mcp at logon (Windows)

`register-task.ps1` creates a scheduled task called **WhatsApp MCP**. When you log on, it starts

```
bin\whatsapp-mcp.exe -read-only -store <StorePath> serve <ExtraArgs>
```

in the repository folder, without a window. The task runs as you and only while you are logged on, so Windows stores no password. If the daemon exits with an error, Task Scheduler restarts it up to 3 times, one minute apart. There is no time limit.

Build the binary first (see [../windows.md](../windows.md)) and pair once with `whatsapp-mcp -store <StorePath> login` or later at `http://127.0.0.1:8765/pair`.

## Register

From the repository folder, in PowerShell:

```powershell
powershell -ExecutionPolicy Bypass -File .\docs\windows\register-task.ps1 -StorePath C:\Users\me\whatsapp-store
```

| Parameter | Default | Meaning |
|---|---|---|
| `-ExePath` | `<repo>\bin\whatsapp-mcp.exe` | the binary |
| `-StorePath` | `<repo>\store` | the store folder (`messages.db`, `whatsapp.db`) |
| `-ExtraArgs` | none | more `serve` flags, added after `serve`, e.g. `"-media-retention 30d"` |
| `-LogPath` | `%LOCALAPPDATA%\whatsapp-mcp\serve.log` | log file |

Running the script again replaces the task with the new settings. It does not start the task. To start it right away, stop any `whatsapp-mcp` you started by hand (only one `serve` can use a store), then:

```powershell
Start-ScheduledTask -TaskName 'WhatsApp MCP'
```

If registering fails with "Access is denied", run the same command once in an elevated PowerShell. The task still runs as your user without admin rights.

## Unregister

```powershell
powershell -ExecutionPolicy Bypass -File .\docs\windows\register-task.ps1 -Unregister
```

This stops the task and removes it. If a `whatsapp-mcp.exe` from the same path is still running afterwards, the script says so and prints the `Stop-Process` command for it.

## Logs

The daemon writes its log to the file from `-LogPath`. Every start adds a `--- start` line, every end a `--- ... exit code` line. Once the file is larger than 10 MB, the next start moves it to `serve.log.1`. To follow it live:

```powershell
Get-Content "$env:LOCALAPPDATA\whatsapp-mcp\serve.log" -Wait -Tail 50
```

Logs are redacted as described in the main README under "Debug logging".

The task itself shows its last run time and result in Task Scheduler (`taskschd.msc`), or with `Get-ScheduledTaskInfo -TaskName 'WhatsApp MCP'`.

## Environment variables

The task does not read a shell profile or a `.env` file. Settings such as `WHATSAPP_MCP_WHISPER_BIN`, `WHATSAPP_MCP_WHISPER_MODEL`, `WHATSAPP_MCP_WHISPER_LANG`, `WHATSAPP_MCP_MEDIA_ROOT` or `WHATSAPP_MCP_ADDR` must be **user environment variables**:

```powershell
[Environment]::SetEnvironmentVariable('WHATSAPP_MCP_WHISPER_BIN', 'C:\tools\whisper\whisper-cli.exe', 'User')
[Environment]::SetEnvironmentVariable('WHATSAPP_MCP_WHISPER_MODEL', 'C:\tools\whisper\ggml-base.bin', 'User')
```

or under Settings > System > About > Advanced system settings > Environment Variables. `ffmpeg` has to be on the user or system `PATH` as well. The task picks up changes the next time it starts: stop and start it, or log off and on.

## How it works

The task runs `conhost.exe --headless`, which keeps the console hidden even when Windows Terminal is the default terminal. conhost starts PowerShell with `serve-task.ps1`, which starts `whatsapp-mcp.exe`, copies its log output into the log file and passes its exit code back to Task Scheduler.
