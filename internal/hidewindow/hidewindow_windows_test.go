package hidewindow

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"syscall"
	"testing"
	"time"
)

func TestApplyPreservesProcessAttributes(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			command := exec.Command("unused")
			var want syscall.SysProcAttr
			if existing {
				want = syscall.SysProcAttr{
					HideWindow: true, CreationFlags: 0x00000200, // CREATE_NEW_PROCESS_GROUP
					CmdLine: "custom command line", NoInheritHandles: true,
					AdditionalInheritedHandles: []syscall.Handle{123},
				}
				attrs := want
				command.SysProcAttr = &attrs
			}
			original := command.SysProcAttr
			want.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
			Apply(command)
			Apply(command) // Applying twice must preserve the same settings.
			if original != nil && command.SysProcAttr != original {
				t.Fatal("replaced existing process attributes")
			}
			if !reflect.DeepEqual(command.SysProcAttr, &want) {
				t.Fatalf("process attributes = %+v, want %+v", command.SysProcAttr, &want)
			}
		})
	}
}

func TestApplyStartsWithoutConsole(t *testing.T) {
	const childEnv = "AMUX_HIDEWINDOW_TEST_CHILD"
	if mode := os.Getenv(childEnv); mode != "" {
		// Query the console window directly. A code-page query does not
		// establish whether the process has an associated console window.
		window, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
		if (window != 0) != (mode == "console") {
			fmt.Fprintf(os.Stderr, "GetConsoleWindow = %#x in %s mode\n", window, mode)
			os.Exit(2)
		}
		fmt.Print("child output")
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"console", "hidden"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestApplyStartsWithoutConsole$")
			command.Env = append(os.Environ(), childEnv+"="+mode)
			if mode == "console" {
				// Positive control: the same probe must detect an explicit console.
				command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000010} // CREATE_NEW_CONSOLE
			} else {
				Apply(command)
			}
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("run child: %v\n%s", err, output)
			}
			if string(output) != "child output" {
				t.Fatalf("child output = %q", output)
			}
		})
	}
}
