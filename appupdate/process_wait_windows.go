//go:build windows

package appupdate

import (
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

func waitForProcessExit(pid int) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(handle)
	result, err := windows.WaitForSingleObject(handle, 2*60*1000)
	if err != nil {
		return err
	}
	if result != uint32(windows.WAIT_OBJECT_0) {
		return fmt.Errorf("timed out waiting for process %d to exit", pid)
	}
	time.Sleep(200 * time.Millisecond)
	return nil
}
