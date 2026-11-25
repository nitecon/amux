//go:build !linux && !darwin && !windows

package worksubmission

import (
	"errors"
	"os"
)

func lockPreferencesFile(*os.File) error {
	return errors.New("work preference locking is unsupported on this OS")
}
