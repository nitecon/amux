package storage

import (
	"fmt"

	"github.com/google/uuid"
)

func CanonicalTaskID(value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil {
		return "", fmt.Errorf("task ID must be a non-zero UUID")
	}
	return id.String(), nil
}
