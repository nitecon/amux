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
	if os.Getenv(childEnv) == "1" {
		consoleCP, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleCP").Call()
		if consoleCP != 0 {
			os.Exit(2)
		}
		fmt.Print("child output without a console")
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestApplyStartsWithoutConsole$")
	command.Env = append(os.Environ(), childEnv+"=1")
	Apply(command)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("run child without console: %v", err)
	}
	if string(output) != "child output without a console" {
		t.Fatalf("child output = %q", output)
	}
}
