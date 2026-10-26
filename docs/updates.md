# App updates

The bell at the top-right of the desktop opens **App updates**. A dot indicates
an applicable newer release or an installed update waiting for restart. Checks
run at startup and hourly; **Check for updates** refreshes manually. A failed
check shows an error and preserves any update already reported.

## Direct downloads

Keep the CLI, desktop and `BUILD.json` from the downloaded bundle together.
Open the top-right bell, or **Preferences → Updates**, and click **Update now**.
The app downloads the release, verifies archive checksums, build metadata and both
executables, then installs the pair. It stays open and keeps your terminals
running. Click **Restart amux** when ready to use the installed version.

There is no command-copy step. The CLI `amux --update` remains available for
scripts and for upgrading older versions that predate the button. On Windows,
files still in use can remain as backup images until a later update can remove
them. Failures report whether restoration was complete; retain any reported
backup files if manual recovery is necessary.

On macOS the app replaces the whole `amux.app` (Apple Silicon releases from
v0.0.28 on). It verifies the archive checksums, `Contents/Resources/BUILD.json`
and the app's code signature seal, then renames the new app into place beside
the old one, so the folder holding `amux.app` (for example `/Applications`)
must be writable. Earlier macOS downloads have no `BUILD.json`; install
v0.0.28 manually once.

Existing review candidates can update to the first later published stable release,
`v0.0.1`. Its installed bundle switches to the stable channel, which follows
increasing `vMAJOR.MINOR.PATCH` tags and ignores previews. A modified or incomplete bundle is not overwritten;
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
closes the current app. Previously running terminal tabs reopen in their saved
directories. Agent tabs use `codex resume --last` or `claude --continue`, following
the provider’s latest conversation in that directory. Tabs whose processes had
already exited remain stopped. Gateway claims and queued terminal input are not
restored, and initialization scripts are not automatically rerun. If saving or starting the replacement
fails, the current app remains open. Some installation methods may require
closing and reopening amux manually.

## Release metadata

Official portable bundles are published at
[GitHub Releases](https://github.com/nitecon/amux/releases/latest) for Linux and
Windows x86_64. Keep every file in the extracted bundle together. For example,
an existing candidate extracted into `~/bin/amux` can be upgraded with:

```sh
cd ~/bin/amux
./amux --update
```

Then use the desktop bell to restart when ready. On Windows, run
`.\amux.exe --update` in PowerShell from the extracted directory.

To publish the next release, bump the package version in `Cargo.toml` and
`Cargo.lock`, commit and push to `main`, then push the matching `vMAJOR.MINOR.PATCH` tag. The native
workflow checks that tag against Cargo, tests and builds both platforms, verifies
the bundles, and publishes both archives and their `SHA256SUMS` only after both
jobs pass. Tagged bundles have `candidate_only: false` in `BUILD.json`.

Update checks read the public
[GitHub releases API](https://docs.github.com/en/rest/releases/releases).
Build identity and platform selection come from the installed bundle metadata;
the update mechanism does not use gateway credentials or agent identities.
Windows running-CLI replacement uses the
[self-replace library](https://docs.rs/self-replace/latest/self_replace/).
