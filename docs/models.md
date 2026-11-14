# Models and native delegation

Open **Settings → Models & Routing** to configure models and thinking levels.
Changes to routing apply to every subsequent task delivery, including queued tasks
not yet pasted. They also accompany review and update notifications; the original
task scope and review restrictions still apply.

| Work | Codex default | Claude default |
| --- | --- | --- |
| Routine code | Luna, medium | Sonnet, medium |
| Research / basic | Terra, medium | Haiku, provider default thinking |
| Complex work | Sol, medium | Opus, medium |

Complexity takes precedence over the routine categories. The injected request asks
the lead to use fresh provider-native subagents, verify their effective model,
and stay conversational where supported. The lead keeps Amux ownership, reviews
results and submits. Helpers must not claim, release, complete or submit tasks.
Amux supplies guidance, not a second scheduler: provider permissions, model access
and native delegation restrictions still apply. A failure must be reported, not
silently replaced with another provider/model. No Amux-owned headless worker pool
is introduced.

## Terminal and persona profiles

Set **Terminal launch default** separately for Codex and Claude. **Provider default**
leaves model and thinking selection to the provider. Override these settings for
an individual terminal/persona in the same panel. **Use provider launch default**
removes the override. Choices persist in this device's preferences by provider and
stable terminal number; closing that terminal and creating another allocates a new
number. These are independent of the models requested for delegated work.

Save settings, then explicitly restart the terminal to apply launch changes.
Changing Settings does not interrupt a running session. Provider arguments are
passed directly, without shell interpolation or global provider configuration edits.

Desktop launches bind native conversation IDs to Amux terminal identities, rather
than resuming the newest conversation in a shared directory. Claude starts with
an explicit `--session-id`; both providers receive a session-only SessionStart hook
that records root conversation transitions, including `/clear`. Forks/helpers do
not replace the lead's binding. Codex asks you to review and trust the
`amux record-native-session` hook in its normal hook UI. Amux does not bypass trust.
If Codex has no recorded binding (including a legacy terminal or an untrusted hook),
it starts fresh with a diagnostic instead of guessing another persona's history.
An explicit saved conversation that cannot resume reports the provider's error.

## Installation catalog

The selectors read `models.json` beside the installed executables on Linux and
Windows, or `amux.app/Contents/Resources/models.json` on macOS. Source debug builds
use `assets/models.json` when no installation catalog exists. The same catalog is
used by routing and terminal selectors. Saved choices remain in user settings.

The JSON format is version 1, with a `models` array. Each entry contains:

```json
{"provider":"codex","id":"gpt-6-luna","label":"Luna","thinking":["low","medium","high","xhigh","max"]}
```

Use provider IDs `codex` or `claude`. An empty `thinking` array means the provider's
default thinking only. Model availability and supported effort depend on the
installed provider and account; the catalog is a maintained list, not an access
check. Update its entries as offerings change. Removed selections stay visible
until corrected; invalid provider/model/effort combinations cannot be saved or
launched. A missing/malformed catalog reports an actionable error.

Platform bundles ship the catalog, and the updater installs it with the application.
An older updater may skip this new data file: run the newly installed `amux --update`
to recover a missing catalog from the verified installed release, or extract the
complete bundle. Existing catalog edits are retained by same-release missing-file
repair; a full application update supplies that release's catalog. macOS catalog
changes affect the app's signature seal; maintain the source catalog and repackage
the application for distribution.

## Validation limits

Native delegation is provider-managed. A bare CLI launch does not report desktop
presence, so CLI model/conversation experiments do not establish automatic queued
assignment or idle-notification behavior. Those paths are covered separately by
native PTY and embedded-server delivery tests. Linux checks with Codex 0.169.0 verified Luna/medium and Terra/medium in native
child turn metadata; the Sol/medium lead answered PONG before Terra completed.
Claude Code 2.1.325 verified Sonnet/medium and Haiku child models; its Opus/medium
lead answered PONG while Haiku continued running. These bounded checks establish
support in those installed versions, not uninterrupted availability in every
provider version or account. Codex hook trust and `/clear` binding were also
verified. Windows/macOS provider interaction still depends on their installed
clients; platform CI covers packaging, argv and Amux delivery contracts.
