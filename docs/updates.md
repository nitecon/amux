# App updates

Use the complete portable bundle from [GitHub Releases](https://github.com/nitecon/amux/releases/latest).
Keep the bundled files together. On Apple Silicon, move the whole `amux.app`
and keep its parent directory writable.

## Update and restart

In the desktop app, open **Settings → Updates** and choose **Check and download
update**. When a verified update is ready, choose **Install and restart**.
Save any work in running sessions before restarting.

If you are still running the previous desktop, use its update bell, choose
**Update now**, then choose **Restart amux**. The first release built from the
rebuild uses the same public release channel and package layout.

From a portable CLI installation, run `amux --update`. When it prints an
`amux --apply-update` command, close the desktop and use that command to
install the verified download. An older CLI may install the update directly.
Restart the desktop to use the installed version.

Use your package manager to upgrade installations it manages. To switch from
a CI candidate to the stable channel, download and install the complete public
bundle. For a same-host gateway migration, follow the
[gateway cutover procedure](../deploy/README.md).

## Release packages

Each stable tag publishes these assets together after all three platform jobs pass:

| Platform | Asset |
| --- | --- |
| Linux x86-64 | `amux-linux-x86_64-vMAJOR.MINOR.PATCH.tar.gz` |
| Windows x86-64 | `amux-windows-x86_64-vMAJOR.MINOR.PATCH.zip` |
| macOS Apple Silicon | `amux-macos-aarch64-vMAJOR.MINOR.PATCH.zip` |
| All platforms | `SHA256SUMS` |

Linux and Windows bundles contain the CLI, desktop, bundled tools, model
catalog and build metadata at the archive root. On macOS they are inside
`amux.app`, which is sealed with an ad-hoc code signature. The updater verifies
the download and package integrity before replacing the installation.

To release, set [VERSION](../VERSION) to the next stable version and push the
matching tag from the validated release commit. The
[packaging workflow](../.github/workflows/go-packaging.yml) checks that the tag
matches `VERSION`, then builds, validates, and publishes a single GitHub release.
