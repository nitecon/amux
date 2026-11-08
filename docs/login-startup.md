# Start amux at sign-in

In **Settings > Workspaces & Terminals**, enable **Start amux when I sign in**
and choose **Save & Close**. The setting is off by default and applies to the
current OS account on this machine. Cancel discards the change.

At the next desktop sign-in, amux opens using its saved preferences. Server mode
starts the existing local gateway; Client mode reconnects to its saved server.
Saved running terminals restore as on a normal launch. Enable **Resume agent
sessions on startup** to resume their provider conversations too.

| Platform | Registration |
| --- | --- |
| Linux | `$XDG_CONFIG_HOME/autostart/amux.desktop` (normally `~/.config/autostart/amux.desktop`) |
| macOS | `~/Library/LaunchAgents/org.amux.desktop.plist`, restricted to the graphical login session |
| Windows | `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Run`, value `amux` |

These native per-user mechanisms need no administrator access. They launch the
installed `amux-desktop` executable directly, with no shell command, project
directory, or credentials in the entry. Keep the CLI and desktop together in a
stable installation directory. After moving the installation, turn the option
off and on and save each change to update the registered path. Updates that
replace binaries in place retain the registration.

Turning the option off removes only amux's registration and takes effect on the
next sign-in. It does not stop the current desktop or gateway. Registration
errors leave Settings open with an error. amux changes registration only when
this setting changes; it does not re-enable startup on every launch or when
other settings are saved. OS policies or startup controls can suppress launch;
Windows may delay startup programs.

This option requires sign-in after reboot. It does not run a gateway before
sign-in, after sign-out, or while the machine is asleep. Those use cases require
a separate system service and lifecycle for the gateway. Graphical terminals
and agents require the user's desktop session. Sandboxed macOS builds cannot
write a LaunchAgent; add the app through **System Settings > General > Login
Items** instead.

The platform choices follow the [XDG autostart specification](https://specifications.freedesktop.org/autostart-spec/latest/),
[Apple's LaunchAgents documentation](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html),
and [Microsoft's Run key documentation](https://learn.microsoft.com/en-us/windows/win32/setupapi/run-and-runonce-registry-keys).
System services (`systemd`, LaunchDaemons, Windows services) have a different
privilege and session model; they are not needed to launch this desktop at login.
