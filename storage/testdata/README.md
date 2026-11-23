`task_status_legacy.sql` is a schema export from the retained Rust gateway's
`db::open`, including tables, explicit indexes and installed triggers. Tests
replace only the task status constraint to exercise both historical variants.
This is a schema fixture with synthetic test rows, not a production backup.

To regenerate, create an isolated Cargo package under `target/` with its own
empty `[workspace]` and an `amux-gateway-backend` path dependency on
`crates/gateway-backend`, with default features disabled. Its main program opens
a new database through `amux_gateway_backend::db::open`, locks the returned
connection, and writes each non-null `sqlite_master.sql` followed by `;`:

```sql
SELECT sql FROM sqlite_master
WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 ELSE 2 END,name
```

Use a new empty database for each export; retain the provenance comments in the
fixture. The Go tests do not need Cargo or Rust at runtime. Re-run the focused
Rust-profile migration tests after refreshing the fixture.

Backup tests enforce POSIX file modes on Unix. Windows file flushing uses a
write-capable handle; its directory-sync and inherited ACL behavior needs native
Windows verification. Cross compilation does not prove these runtime properties.
