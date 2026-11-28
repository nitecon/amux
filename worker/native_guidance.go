package worker

import "os"

func nativeMCPEnvironment() map[string]string {
	value := os.Getenv("AMUX_DESKTOP_GUIDANCE")
	if value == "" {
		return nil
	}
	return map[string]string{"AMUX_DESKTOP_GUIDANCE": value}
}
