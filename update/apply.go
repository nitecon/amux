package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nitecon/amux/update/platform"
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
	if err := CheckInstallable(executable); err != nil {
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
	commandArgs := []string{"update", "--staged", archivePath, "--executable", executable,
		"--parent-pid", fmt.Sprint(parentPID), "--wait-pids", string(waitJSON), "--restart-args", string(argsJSON), "--cache-root", cacheRoot}
	command := exec.Command(helper, commandArgs...)
	native.PrepareHelper(command)
	command.Stdout = nil
	command.Stderr = nil
	if err := command.Start(); err != nil {
		return err
	}
	return nil
}

// FinishHelper is an internal command used by the temporary updater copy.
func FinishHelper(args []string) error {
	request, err := parseHelperArgs(args)
	if err != nil {
		if len(args) > 1 {
			recordResult(args[1], "The amux update was not installed: "+err.Error())
		}
		return err
	}
	return finishUpdate(request, native.WaitForProcessExit, launchDesktop)
}

func recordResult(archivePath, message string) {
	_ = os.WriteFile(filepath.Join(filepath.Dir(archivePath), "install.log"), []byte(message+"\n"), 0o600)
}

// TakeFailure returns the report of an update that failed after the desktop
// closed. Each report is returned once; its log is kept as install.reported.log.
func TakeFailure() string {
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return takeFailureAt(cacheRoot)
}

func takeFailureAt(cacheRoot string) string {
	logs, _ := filepath.Glob(filepath.Join(cacheRoot, "amux", "updates", "*", "install.log"))
	failure := ""
	var newest time.Time
	for _, log := range logs {
		info, statErr := os.Stat(log)
		data, err := os.ReadFile(log)
		if statErr != nil || err != nil || os.Rename(log, filepath.Join(filepath.Dir(log), "install.reported.log")) != nil {
			continue
		}
		// Only the latest attempt counts: a later success supersedes an older failure.
		if !info.ModTime().Before(newest) {
			newest = info.ModTime()
			failure = strings.TrimSpace(string(data))
			if failure == "update completed" {
				failure = ""
			}
		}
	}
	return failure
}

type helperRequest struct {
	archivePath, executable, cacheRoot string
	pids                               []int
	restartArgs                        []string
}

func parseHelperArgs(args []string) (helperRequest, error) {
	var empty helperRequest
	if len(args) != 7 {
		return empty, errors.New("invalid internal updater arguments")
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return empty, err
	}
	wantCacheRoot, err := filepath.Abs(cacheRoot)
	if err != nil {
		return empty, err
	}
	providedCacheRoot, err := filepath.Abs(args[6])
	if err != nil {
		return empty, err
	}
	if providedCacheRoot != wantCacheRoot {
		executablePath, pathErr := filepath.Abs(args[2])
		tempRoot, tempErr := filepath.Abs(os.TempDir())
		if pathErr != nil || tempErr != nil {
			return empty, errors.New("could not validate isolated update paths")
		}
		relative, relErr := filepath.Rel(tempRoot, executablePath)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return empty, errors.New("non-user cache root is allowed only for temporary test installs")
		}
	}
	request := helperRequest{archivePath: args[1], executable: args[2], cacheRoot: args[6]}
	parentPID, err := optionalPID(args[3])
	if err != nil {
		return empty, err
	}
	if err := json.Unmarshal([]byte(args[4]), &request.pids); err != nil {
		return empty, errors.New("invalid helper process wait list")
	}
	for _, pid := range request.pids {
		if pid <= 0 {
			return empty, errors.New("invalid helper process id")
		}
	}
	if err := json.Unmarshal([]byte(args[5]), &request.restartArgs); err != nil {
		return empty, err
	}
	if parentPID > 0 {
		request.pids = append(request.pids, parentPID)
	}
	return request, nil
}

// finishUpdate records the outcome before reopening the desktop, because the
// desktop reads it once at startup.
func finishUpdate(request helperRequest, wait func(int) error, launch func(string, []string) error) error {
	for _, pid := range request.pids {
		if err := wait(pid); err != nil {
			// That process may be the desktop, so do not open a second one. The
			// report is shown the next time the desktop starts.
			recordResult(request.archivePath, "The amux update was not installed: "+err.Error())
			return err
		}
	}
	_, installErr := applyStagedAt(request.executable, request.archivePath, request.cacheRoot)
	message := "update completed"
	if installErr != nil {
		message = "The amux update was not installed: " + installErr.Error()
	}
	recordResult(request.archivePath, message)
	if request.restartArgs == nil {
		return installErr
	}
	// After a failed install this reopens the untouched previous version.
	if err := launch(request.executable, request.restartArgs); err != nil {
		if installErr == nil {
			message = "The amux update was installed, but amux could not restart: " + err.Error()
		} else {
			message += ". amux also could not reopen: " + err.Error()
		}
		// Kept for the next time the user opens the desktop.
		recordResult(request.archivePath, message)
		return errors.Join(installErr, fmt.Errorf("desktop restart failed: %w", err))
	}
	return installErr
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
	dir, _, _, _, err := updateInstallPaths(executable)
	if err != nil {
		return err
	}
	return ensureNewerInstall(dir, build)
}

func ensureNewerInstall(dir string, build Build) error {
	installed, err := readInstalled(dir)
	if err == nil && (compareVersions(build.Tag, installed.Tag) <= 0 || build.Revision == installed.Revision) {
		return errors.New("staged update is not newer than the installed version")
	}
	return nil
}

// Resolve the installation directory before replacing files, but require that
// both executable entries themselves are regular files in that directory.
func updateInstallPaths(executable string) (dir, app, localCLI, localGUI string, err error) {
	_, cli, gui, _, err := target()
	if err != nil {
		return "", "", "", "", err
	}
	localCLI, localGUI = native.LocalNames(executable, cli, gui)
	if !strings.EqualFold(filepath.Base(executable), localCLI) {
		return "", "", "", "", errors.New("update target must be the installed amux CLI")
	}
	dir, err = filepath.Abs(filepath.Dir(executable))
	if err != nil {
		return "", "", "", "", err
	}
	bundle, hasBundle := native.(bundleUpdater)
	if err := checkInstallRestrictions(dir); err != nil {
		return "", "", "", "", err
	}
	app = appBundle(dir)
	directories := []string{dir}
	if app != "" {
		directories = append(directories, filepath.Dir(dir), app)
	}
	for _, directory := range directories {
		if info, err := os.Lstat(directory); err != nil || !info.IsDir() {
			return "", "", "", "", errors.New("amux installation must use real directories")
		}
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", "", "", err
	}
	if app != "" && appBundle(resolved) == "" {
		return "", "", "", "", errors.New("amux app bundle executable directory is redirected")
	}
	dir = resolved
	app = appBundle(dir)
	if err := checkInstallRestrictions(dir); err != nil {
		return "", "", "", "", err
	}
	for _, name := range []string{localCLI, localGUI} {
		if info, err := os.Lstat(filepath.Join(dir, name)); err != nil || !info.Mode().IsRegular() {
			return "", "", "", "", fmt.Errorf("update target is missing regular file %s", name)
		}
	}
	if hasBundle {
		if err := bundle.CheckDestination(dir, app); err != nil {
			return "", "", "", "", err
		}
	}
	return dir, app, localCLI, localGUI, nil
}

// CheckInstallable reports problems the helper would otherwise only hit after
// the desktop has already closed.
func CheckInstallable(executable string) error {
	dir, app, localCLI, localGUI, err := updateInstallPaths(executable)
	if err != nil {
		return err
	}
	stageParent := dir
	if app != "" {
		stageParent = filepath.Dir(app)
	}
	lock, err := acquireUpdateLock(stageParent)
	if err == nil {
		// An existing lock file opens even where new entries are refused, so
		// create the staging directory the helper needs.
		var probe string
		if probe, err = os.MkdirTemp(stageParent, ".amux-update-stage-"); err == nil {
			err = os.Remove(probe)
		}
		err = errors.Join(err, lock.Close())
	}
	if errors.Is(err, fs.ErrPermission) {
		fix := native.WriteAccessAdvice()
		return fmt.Errorf("your user cannot write to the amux installation directory %s. %s, or move amux to a directory you own, then update again", stageParent, fix)
	}
	if err != nil {
		return fmt.Errorf("amux cannot update the installation in %s: %w", stageParent, err)
	}
	if _, ok := native.(bundleUpdater); ok {
		return nil
	}
	triple, _, _, _, err := target()
	if err != nil {
		return err
	}
	for _, name := range installFileNames(triple, localCLI, localGUI) {
		if info, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) && (err != nil || !info.Mode().IsRegular()) {
			return fmt.Errorf("installed file %s in %s is not a regular file", name, dir)
		}
	}
	return nil
}

func installFileNames(triple, localCLI, localGUI string) []string {
	files := append([]string{localCLI, localGUI}, bundledToolNames(triple)...)
	return append(files, "models.json", "LICENSE", "BUILD.json", "SHA256SUMS")
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
	dir, app, localCLI, localGUI, err := updateInstallPaths(executable)
	if err != nil {
		return empty, err
	}
	stageParent := dir
	if app != "" {
		stageParent = filepath.Dir(app)
	}
	lock, err := acquireUpdateLock(stageParent)
	if err != nil {
		return empty, err
	}
	defer lock.Close()
	stageDir, err := os.MkdirTemp(stageParent, ".amux-update-stage-")
	if err != nil {
		return empty, err
	}
	defer os.RemoveAll(stageDir)
	build, err := verifyArchiveAt(data, stageDir, metadata.Tag, triple)
	if err != nil {
		return empty, err
	}
	if err := ensureNewerInstall(dir, build); err != nil {
		return empty, err
	}
	if bundle, ok := native.(bundleUpdater); ok {
		return build, bundle.InstallBundle(dir, app, stageDir)
	}
	if cli != localCLI {
		if err := os.Rename(filepath.Join(stageDir, cli), filepath.Join(stageDir, localCLI)); err != nil {
			return empty, err
		}
		if err := os.Rename(filepath.Join(stageDir, gui), filepath.Join(stageDir, localGUI)); err != nil {
			return empty, err
		}
	}
	if err := installFiles(dir, stageDir, installFileNames(triple, localCLI, localGUI), func() error { return verifyInstalled(dir, build, localCLI, localGUI) }, os.Rename); err != nil {
		return empty, err
	}
	return build, nil
}

func acquireUpdateLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, ".amux-update.lock")
	if info, err := os.Lstat(path); !os.IsNotExist(err) && (err != nil || !info.Mode().IsRegular()) {
		return nil, errors.New("update lock must be a regular file")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open update lock: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("update lock must be a regular file")
	}
	if err := native.LockUpdateFile(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("cannot acquire update lock: %w", err)
	}
	return file, nil
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
		if !os.IsNotExist(err) {
			if err != nil || !info.Mode().IsRegular() {
				return platform.RollbackFailure(backupDir, rollback(), fmt.Errorf("installed file %s is not regular", name))
			}
			if err := rename(destination, filepath.Join(backupDir, name)); err != nil {
				return platform.RollbackFailure(backupDir, rollback(), fmt.Errorf("cannot stage old %s: %w", name, err))
			}
			backedUp = append(backedUp, name)
		}
		if err := rename(filepath.Join(stageDir, name), destination); err != nil {
			return platform.RollbackFailure(backupDir, rollback(), fmt.Errorf("cannot install %s: %w", name, err))
		}
		installedNew = append(installedNew, name)
	}
	if err := verify(); err != nil {
		return platform.RollbackFailure(backupDir, rollback(), fmt.Errorf("new bundle failed verification: %w", err))
	}
	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("update installed, but old files remain in %s: %w", backupDir, err)
	}
	return nil
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

func copyHelper(executable string) (string, error) {
	input, err := os.Open(executable)
	if err != nil {
		return "", err
	}
	defer input.Close()
	file, err := os.CreateTemp("", native.HelperPattern())
	if err != nil {
		return "", err
	}
	path := file.Name()
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
	_, cli, gui, _, err := target()
	if err != nil {
		return err
	}
	dir := filepath.Dir(executable)
	if bundle, ok := native.(bundleUpdater); ok && bundle.AppBundle(dir) == "" {
		dir = bundle.ArchiveBinaryDir(dir)
	}
	_, gui = native.LocalNames(executable, cli, gui)
	return RelaunchDesktop(filepath.Join(dir, gui), args)
}

// RelaunchDesktop starts the desktop as a new application instance.
func RelaunchDesktop(executable string, args []string) error {
	return native.Relaunch(executable, args)
}
