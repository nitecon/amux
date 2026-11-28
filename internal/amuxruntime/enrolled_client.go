package amuxruntime

import "path/filepath"

func absoluteEnrolledManifest(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return absolute, nil
}
