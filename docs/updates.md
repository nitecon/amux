# App updates

Use the complete portable bundle from [GitHub Releases](https://github.com/nitecon/amux/releases/latest).
Keep the bundled files together. On Apple Silicon, move the whole `amux.app`
and keep its parent directory writable.

## Update and restart

In the desktop app, save any work in running sessions, open **Settings → Updates**,
and choose **Update now**. Amux downloads and verifies the package for your
platform, installs it, and restarts automatically.

The installation directory must be writable by your user and hold the bundle's
own files. If it is not, for example a root-owned copy in `/usr/local/bin` or
bundled tools replaced by links, amux reports the reason and keeps running
without changing anything. If your user cannot write to the directory, give your
user write access on Windows or make your user its owner with `chown` on Linux
and macOS. You can also move amux to a directory you own, then update again.

If the installation fails after the desktop has closed, amux reopens the
previous version and shows the reason. If amux cannot reopen, the reason is
shown the next time you open the desktop. The helper writes the report to
`install.log` beside the downloaded package in the amux update cache. The
desktop keeps it as `install.reported.log` once it shows the failure.

The upfront checks and desktop failure notice start with the release after
v0.9.1. In the Go desktop releases v0.9.0 and v0.9.1, amux says it is
restarting, then closes so the update helper can install the package. If
installation fails, amux stays closed and the old version is still installed
when you open it again. The helper records the reason in `install.log` beside
the downloaded package in the amux update cache.

Before updating from v0.9.0 or v0.9.1, check that your user can write to the
installation directory. Give your user write access on Windows, use `chown`
to make your user the owner on Linux and macOS, or move amux to a directory
you own. You can also close amux and rerun the
[install script](../README.md#get-started), which installs the latest release
in your home directory. The install script does not remove the old
copy: a `/usr/local/bin/amux` stays on your `PATH`, and a systemd service
started from it (see the [deployment guide](../deploy/README.md)) keeps running
the old version. After moving to the home-directory installation, stop that
service so nothing connects to the old server. Then remove the old copy, or put
`~/amux` first on your `PATH`, so you launch the new installation.

If you are still running the previous desktop, use its update bell, choose
**Update now**, then choose **Restart amux**. The first release built from the
rebuild uses the same public release channel and package layout.

From a portable CLI installation, close the desktop and any running local
amux client, then run `amux --update`. The helper installs the verified update
after the command exits. Launch the desktop to use the installed version.

You can also rerun the [install script](../README.md#get-started) for your
platform. It replaces only an installation at its default location (`~/amux`,
`~/Applications/amux.app` or `%LOCALAPPDATA%\amux`) with the latest release.
For an installation elsewhere, use **Settings → Updates** or `amux --update`.

Source builds with the CLI and desktop together in a writable directory can
also update to the stable release. A loose macOS source build installs and opens
the signed `amux.app` beside the source binaries.

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
