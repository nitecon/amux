package appupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// StartApply schedules a verified update helper, then lets the running desktop
// and CLI exit before any installed executable is replaced.
func StartApply(executable, archivePath string, waitPIDs []int, restartArgs []string) error {
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	return startApplyAt(executable, archivePath, cacheRoot, waitPIDs, restartArgs, os.Getpid())
}

func startApplyAt(executable, archivePath, cacheRoot string, waitPIDs []int, restartArgs []string, parentPID int) error {
	info, err := os.Stat(archivePath)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("staged update archive is missing or not a regular file")
	}
	verified, err := stagePathAt(archivePath, cacheRoot)
	if err != nil {
		return err
	}
	if err := verifyStageAt(executable, archivePath, verified, cacheRoot); err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	for _, pid := range waitPIDs {
		if pid <= 0 {
			return errors.New("invalid process wait id")
		}
	}
	argsJSON, err := json.Marshal(restartArgs)
	if err != nil {
		return err
	}
	helper, err := copyHelper(executable)
	if err != nil {
		return err
	}
	waitJSON, err := json.Marshal(waitPIDs)
	if err != nil {
		return err
	}
	commandArgs := []string{"--finish-update", archivePath, executable, fmt.Sprint(parentPID), string(waitJSON), string(argsJSON), cacheRoot}
	command := exec.Command(helper, commandArgs...)
	command.Stdout = nil
	command.Stderr = nil
	if err := command.Start(); err != nil {
		return err
	}
	return nil
}

// FinishHelper is an internal command used by the temporary updater copy.
func FinishHelper(args []string) error {
	err := finishHelper(args)
	if len(args) > 1 {
		message := "update completed"
		if err != nil {
			message = err.Error()
		}
		_ = os.WriteFile(filepath.Join(filepath.Dir(args[1]), "install.log"), []byte(message+"\n"), 0o600)
	}
	return err
}

func finishHelper(args []string) error {
	if len(args) != 7 {
		return errors.New("invalid internal updater arguments")
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	wantCacheRoot, err := filepath.Abs(cacheRoot)
	if err != nil {
		return err
	}
	providedCacheRoot, err := filepath.Abs(args[6])
	if err != nil {
		return err
	}
	if providedCacheRoot != wantCacheRoot {
		executablePath, pathErr := filepath.Abs(args[2])
		tempRoot, tempErr := filepath.Abs(os.TempDir())
		if pathErr != nil || tempErr != nil {
			return errors.New("could not validate isolated update paths")
		}
		relative, relErr := filepath.Rel(tempRoot, executablePath)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("non-user cache root is allowed only for temporary test installs")
		}
	}
	archivePath, executable := args[1], args[2]
	parentPID, err := optionalPID(args[3])
	if err != nil {
		return err
	}
	var waitPIDs []int
	if err := json.Unmarshal([]byte(args[4]), &waitPIDs); err != nil {
		return errors.New("invalid helper process wait list")
	}
	for _, pid := range waitPIDs {
		if pid <= 0 {
			return errors.New("invalid helper process id")
		}
	}
	var restartArgs []string
	if err := json.Unmarshal([]byte(args[5]), &restartArgs); err != nil {
		return err
	}
	pids := waitPIDs
	if parentPID > 0 {
		pids = append(pids, parentPID)
	}
	for _, pid := range pids {
		if pid == 0 {
			continue
		}
		if err := waitForProcessExit(pid); err != nil {
			return err
		}
	}
	build, err := applyStagedAt(executable, archivePath, args[6])
	if err != nil {
		return err
	}
	if restartArgs != nil {
		gui := filepath.Join(filepath.Dir(executable), build.GUIBinary)
		command := exec.Command(gui, restartArgs...)
		if err := command.Start(); err != nil {
			return fmt.Errorf("update installed, but desktop restart failed: %w", err)
		}
	}
	return nil
}

func stagePath(archivePath string) (stageMetadata, error) {
	var metadata stageMetadata
	cache, err := os.UserCacheDir()
	if err != nil {
		return metadata, err
	}
	return stagePathAt(archivePath, cache)
}

func stagePathAt(archivePath, cacheRoot string) (stageMetadata, error) {
	var metadata stageMetadata
	updatesRoot := filepath.Join(cacheRoot, "amux", "updates")
	archiveAbs, err := filepath.Abs(archivePath)
	if err != nil {
		return metadata, err
	}
	relative, err := filepath.Rel(updatesRoot, archiveAbs)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return metadata, errors.New("staged update must come from the private amux update cache")
	}
	info, err := os.Lstat(filepath.Join(filepath.Dir(archiveAbs), "verified.json"))
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return metadata, errors.New("verified update metadata is missing")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(archiveAbs), "verified.json"))
	if err != nil || json.Unmarshal(data, &metadata) != nil || !versionPattern.MatchString(metadata.Tag) || !validDigest("sha256:"+metadata.Digest) {
		return metadata, errors.New("invalid verified update metadata")
	}
	return metadata, nil
}

func verifyStage(executable, archivePath string, metadata stageMetadata) error {
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	return verifyStageAt(executable, archivePath, metadata, cacheRoot)
}

func verifyStageAt(executable, archivePath string, metadata stageMetadata, cacheRoot string) error {
	data, err := readBoundedArchive(archivePath)
	if err != nil {
		return err
	}
	if hashBytes(data) != metadata.Digest {
		return errors.New("staged update archive has changed since verification")
	}
	triple, _, _, _, err := target()
	if err != nil {
		return err
	}
	build, err := verifyArchive(data, metadata.Tag, triple)
	if err != nil {
		return err
	}
	installed, err := readInstalled(filepath.Dir(executable))
	if err != nil {
		return fmt.Errorf("installed bundle metadata: %w", err)
	}
	if compareVersions(build.Tag, installed.Tag) <= 0 || build.Revision == installed.Revision {
		return errors.New("staged update is not newer than the installed version")
	}
	return verifyInstalled(filepath.Dir(executable), installed, func() string {
		if runtime.GOOS == "windows" {
			return "amux.exe"
		}
		return "amux"
	}(), installed.GUIBinary)
}

func applyStaged(executable, archivePath string) (Build, error) {
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return Build{}, err
	}
	return applyStagedAt(executable, archivePath, cacheRoot)
}

func applyStagedAt(executable, archivePath, cacheRoot string) (Build, error) {
	var empty Build
	metadata, err := stagePathAt(archivePath, cacheRoot)
	if err != nil {
		return empty, err
	}
	data, err := readBoundedArchive(archivePath)
	if err != nil {
		return empty, err
	}
	if hashBytes(data) != metadata.Digest {
		return empty, errors.New("staged update archive has changed since verification")
	}
	triple, cli, gui, _, err := target()
	if err != nil {
		return empty, err
	}
	build, err := verifyArchive(data, metadata.Tag, triple)
	if err != nil {
		return empty, err
	}
	dir := filepath.Dir(executable)
	lockPath := filepath.Join(dir, ".amux-update.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return empty, fmt.Errorf("another update is in progress or the update lock needs cleanup: %w", err)
	}
	defer func() { _ = lock.Close(); _ = os.Remove(lockPath) }()
	installed, err := readInstalled(dir)
	if err != nil {
		return empty, err
	}
	if compareVersions(build.Tag, installed.Tag) <= 0 || build.Revision == installed.Revision {
		return empty, errors.New("staged update is not newer than the installed version")
	}
	if err := verifyInstalled(dir, installed, cli, gui); err != nil {
		return empty, fmt.Errorf("installed bundle failed verification: %w", err)
	}
	stageDir, err := os.MkdirTemp(dir, ".amux-update-stage-")
	if err != nil {
		return empty, err
	}
	defer os.RemoveAll(stageDir)
	if err := extractVerified(data, stageDir); err != nil {
		return empty, err
	}
	files := []string{cli, gui}
	if runtime.GOOS == "windows" {
		files = append(files, "agent-tools.exe", "memory.exe")
	} else {
		files = append(files, "agent-tools", "memory")
	}
	files = append(files, "models.json", "LICENSE", "BUILD.json", "SHA256SUMS")
	if err := installFiles(dir, stageDir, files, func() error { return verifyInstalled(dir, build, cli, gui) }, os.Rename); err != nil {
		return empty, err
	}
	return build, nil
}

func installFiles(dir, stageDir string, files []string, verify func() error, rename func(string, string) error) error {
	backupDir, err := os.MkdirTemp(dir, ".amux-update-backup-")
	if err != nil {
		return err
	}
	backedUp := make([]string, 0, len(files))
	installedNew := make([]string, 0, len(files))
	rollback := func() error {
		var rollbackErrors []error
		for index := len(installedNew) - 1; index >= 0; index-- {
			if err := os.Remove(filepath.Join(dir, installedNew[index])); err != nil && !os.IsNotExist(err) {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("remove new %s: %w", installedNew[index], err))
			}
		}
		for index := len(backedUp) - 1; index >= 0; index-- {
			name := backedUp[index]
			if err := rename(filepath.Join(backupDir, name), filepath.Join(dir, name)); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("restore %s: %w", name, err))
			}
		}
		return errors.Join(rollbackErrors...)
	}
	for _, name := range files {
		destination := filepath.Join(dir, name)
		info, err := os.Lstat(destination)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return rollbackFailure(backupDir, rollback(), fmt.Errorf("installed file %s is missing or not regular", name))
		}
		if err := rename(destination, filepath.Join(backupDir, name)); err != nil {
			return rollbackFailure(backupDir, rollback(), fmt.Errorf("cannot stage old %s: %w", name, err))
		}
		backedUp = append(backedUp, name)
		if err := rename(filepath.Join(stageDir, name), destination); err != nil {
			return rollbackFailure(backupDir, rollback(), fmt.Errorf("cannot install %s: %w", name, err))
		}
		installedNew = append(installedNew, name)
	}
	if err := verify(); err != nil {
		return rollbackFailure(backupDir, rollback(), fmt.Errorf("new bundle failed verification: %w", err))
	}
	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("update installed, but old files remain in %s: %w", backupDir, err)
	}
	return nil
}

func rollbackFailure(backupDir string, rollbackErr, originalErr error) error {
	if rollbackErr != nil {
		return fmt.Errorf("%w; rollback also failed: %v; previous files are retained in %s", originalErr, rollbackErr, backupDir)
	}
	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("%w; rollback completed, but backup cleanup failed at %s: %v", originalErr, backupDir, err)
	}
	return originalErr
}

func readBoundedArchive(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("staged update archive is not a regular file")
	}
	if info.Size() <= 0 || info.Size() > maxReleaseBody {
		return nil, errors.New("staged update archive exceeds the allowed size")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxReleaseBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != info.Size() {
		return nil, errors.New("staged update archive changed while being read")
	}
	return data, nil
}

func extractVerified(data []byte, destination string) error {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer reader.Close()
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		mode := os.FileMode(header.Mode) & 0o755
		if mode == 0 {
			mode = 0o600
		}
		file, err := os.OpenFile(filepath.Join(destination, header.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(file, archive, header.Size)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
}

func copyHelper(executable string) (string, error) {
	input, err := os.Open(executable)
	if err != nil {
		return "", err
	}
	defer input.Close()
	file, err := os.CreateTemp("", "amux-update-helper-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if runtime.GOOS == "windows" {
		path += ".exe"
		_ = file.Close()
		_ = os.Remove(file.Name())
		file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
		if err != nil {
			return "", err
		}
	}
	if _, err := io.Copy(file, input); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Chmod(0o700); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func launchDesktop(executable string, args []string) error {
	_, _, gui, _, err := target()
	if err != nil {
		return err
	}
	command := exec.Command(filepath.Join(filepath.Dir(executable), gui), args...)
	return command.Start()
}
