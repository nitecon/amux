# Rules
- Never ever make use of an internal package.
  - Packages must be created with proper names and should not stutter when expanded.
- A package must have it's entrypoint/interface defined in the <package>/<packagename.go> for instance: update/update.go (the exception is main.go)
- Use zerolog for Go application logging.
- Define CLI commands and subcommands with urfave/cli v3.
