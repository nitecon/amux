package platform

import (
	"fmt"
	"os"
	"os/exec"
)

// Flat supplies the behavior shared by standalone executable installs.
type Flat struct{}

func (Flat) LocalNames(_, cli, gui string) (string, string) { return cli, gui }
func (Flat) HelperPattern() string                          { return "amux-handoff-*" }
func (Flat) PrepareHelper(*exec.Cmd)                        {}

func (Flat) WriteAccessAdvice() string {
	return "Make your user the owner of that directory with chown"
}

func (Flat) Relaunch(executable string, args []string) error {
	command := exec.Command(executable, args...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	_ = command.Process.Release()
	return nil
}

func RollbackFailure(backupDir string, rollbackErr, originalErr error) error {
	if rollbackErr != nil {
		return fmt.Errorf("%w; rollback also failed: %v; previous files are retained in %s", originalErr, rollbackErr, backupDir)
	}
	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("%w; rollback completed, but backup cleanup failed at %s: %v", originalErr, backupDir, err)
	}
	return originalErr
}
