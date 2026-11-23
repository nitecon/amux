package appupdate

import (
	"fmt"
	"strconv"
)

func positivePID(value string) (int, error) {
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid process id")
	}
	return pid, nil
}

func optionalPID(value string) (int, error) {
	if value == "0" {
		return 0, nil
	}
	return positivePID(value)
}
