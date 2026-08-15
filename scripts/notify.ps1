# Windows-side notification bridge for WSL2 Claude Code sessions.
# Triggered by the Notification hook in .claude/settings.json when Claude
# is blocked on a permission prompt or question, or when /loop and /goal
# go idle between iterations.
#
# Only pops a Windows balloon if a terminal emulator is NOT in the
# foreground. If the user is actively in the terminal, the bell from
# preferredNotifChannel=terminal_bell is enough — an extra popup would
# be noise. If they're elsewhere, the balloon catches their eye.

$ErrorActionPreference = 'SilentlyContinue'

# Win32 interop: identify which process owns the foreground window so we
# can tell whether a terminal emulator currently has focus.
Add-Type -TypeDefinition @'
using System;
using System.Diagnostics;
using System.Runtime.InteropServices;
public class WinFocus {
    [DllImport("user32.dll")]
    public static extern IntPtr GetForegroundWindow();
    [DllImport("user32.dll")]
    static extern uint GetWindowThreadProcessId(IntPtr hWnd, out uint lpdwProcessId);
    public static string ForegroundProcessName() {
        IntPtr h = GetForegroundWindow();
        if (h == IntPtr.Zero) return "";
        try {
            uint pid;
            GetWindowThreadProcessId(h, out pid);
            return Process.GetProcessById((int)pid).ProcessName.ToLowerInvariant();
        } catch { return ""; }
    }
}
'@

$fg = [WinFocus]::ForegroundProcessName()

# Common WSL/Windows terminal hosts. Match by process name (lowercased).
$terminals = @(
    'windowsterminal',   # Windows Terminal
    'wt',                # Windows Terminal alias
    'cmd',               # Command Prompt
    'powershell',        # Windows PowerShell 5.x
    'pwsh',              # PowerShell 7+
    'conhost',           # Console Host (conhost.exe fallback)
    'mintty',            # mintty (Git Bash, Cygwin, MSYS2)
    'wezterm',           # WezTerm
    'alacritty',         # Alacritty
    'code',              # VS Code integrated terminal
    'code - insiders'    # VS Code Insiders integrated terminal
)

if ($terminals -contains $fg) {
    # Terminal owns the foreground — the in-terminal bell from
    # preferredNotifChannel=terminal_bell is the signal. Stay quiet.
    exit 0
}

# Terminal NOT in foreground — pop a Windows balloon. Non-modal,
# auto-dismisses after 10s, blocks until dismissed or timed out.
(New-Object -ComObject WScript.Shell).Popup(
    'Claude Code needs your attention',
    10,
    'Claude Code',
    64
) | Out-Null