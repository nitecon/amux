//go:build windows

package storage

// Windows has no equivalent POSIX directory-fsync contract. Backup durability
// relies on syncing the backup file and SQLite's VFS baseline; this does not
// promise stronger power-loss guarantees. It does not flush a volume or need
// volume-level administrative privileges.
func syncTaskStatusBackupDirectory(string) error { return nil }
