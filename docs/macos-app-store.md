# macOS client and Mac App Store

CI job `macos` (macos-15, Apple Silicon) builds `amux.app` with
`scripts/package_macos.sh`. The bundle holds `amux-desktop` (the app
executable) and the `amux` CLI side by side in `Contents/MacOS`.

## Unsigned builds (current)

Without signing secrets the app is ad-hoc signed and uploaded as the
`amux-aarch64-apple-darwin-<sha>` workflow artifact
(`amux-macos-aarch64-v<version>.zip`). Gatekeeper blocks it on first launch;
right-click → Open, or `xattr -dr com.apple.quarantine amux.app`.

## Enabling App Store signing

When the Apple Developer agreements are renewed, create these repository
secrets. Signing, the installer `.pkg` and upload (tags only) switch on when
`MAS_APP_IDENTITY` is set.

| Secret | Value |
| --- | --- |
| `MAS_APP_IDENTITY` | `3rd Party Mac Developer Application: <Team> (<TEAMID>)` |
| `MAS_INSTALLER_IDENTITY` | `3rd Party Mac Developer Installer: <Team> (<TEAMID>)` |
| `MAS_APP_CERTIFICATE` | base64 of the application certificate `.p12` |
| `MAS_INSTALLER_CERTIFICATE` | base64 of the installer certificate `.p12` |
| `MAS_CERTIFICATE_PASSWORD` | password of both `.p12` files |
| `MAS_PROVISIONING_PROFILE` | base64 of the Mac App Store provisioning profile |
| `APP_STORE_CONNECT_KEY_ID` | App Store Connect API key ID |
| `APP_STORE_CONNECT_ISSUER_ID` | App Store Connect issuer ID |
| `APP_STORE_CONNECT_KEY` | contents of the `AuthKey_<id>.p8` file |

The bundle identifier defaults to `com.nitecon.amux`; set `MACOS_BUNDLE_ID`
in the job to match the App Store Connect record.

## Known App Store constraints

- **App Sandbox is mandatory** (`packaging/macos/amux.entitlements`). Shells
  and agents started by amux inherit the sandbox, so they can only reach the
  app container and user-selected folders, not arbitrary project directories,
  `~/.claude` or tools installed elsewhere. Expect to verify, and likely
  adapt, terminal and agent behavior once a signed sandboxed build exists.
- App Store installs must not self-update. They have no `BUILD.json`, so
  `amux --update` already treats them as unmanaged and changes nothing.
- The store listing needs a 1024×1024 icon; `assets/icons/amux.png` is
  256×256 and is upscaled for now.
