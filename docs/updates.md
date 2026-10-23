# App updates

The bell at the top-right of the desktop opens **App updates**. A dot indicates
an applicable newer release or an installed update waiting for restart. Checks
run at startup and hourly; **Check for updates** refreshes manually. A failed
check shows an error and preserves any update already reported.

## Direct downloads

Keep the CLI, desktop and `BUILD.json` from the downloaded bundle together.
The update panel shows the available release and a command you can copy:

```sh
amux --update
```

In PowerShell, use `.\amux.exe --update` from the extracted directory if amux
is not on PATH. The command verifies release checksums, build metadata and both
executables before replacing the installed pair. It does not stop running
terminals or agents. On Windows, files still in use can remain as backup images
until a later update can remove them. Failures report whether restoration was
complete; retain any reported backup files if manual recovery is necessary.

Review candidates follow later published review candidates. Stable installs
follow stable version tags. A modified or incomplete bundle is not overwritten;
download and extract a complete bundle instead. If the installed preview cannot
be matched to published release metadata, the app reports that limitation
rather than guessing an update order from commit hashes.

## Package managers

Homebrew installs show the Homebrew upgrade command. Other installations show
manual guidance to use the tool that installed amux. `amux --update` does not
overwrite package-manager files. Installing a package update and restarting the
running app are separate steps.

## Restart

The desktop checks for an installed replacement. When one is ready, the update
panel offers **Restart amux**, including the number of running sessions it will
close. Restart is explicit: it saves the layout, starts the updated desktop and
closes the current app. Restored terminal and agent tabs remain stopped; no
previous work is automatically rerun. If saving or starting the replacement
fails, the current app remains open. Some installation methods may require
closing and reopening amux manually.

## Release metadata

Update checks read the public
[GitHub releases API](https://docs.github.com/en/rest/releases/releases).
Build identity and platform selection come from the installed bundle metadata;
the update mechanism does not use gateway credentials or agent identities.
Windows running-CLI replacement uses the
[self-replace library](https://docs.rs/self-replace/latest/self_replace/).
