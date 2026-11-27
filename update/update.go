package update

import (
	"os"
	"os/exec"
)

type updaterPlatform interface {
	LockUpdateFile(*os.File) error
	WaitForProcessExit(int) error
	LocalNames(executable, cli, gui string) (string, string)
	HelperPattern() string
	PrepareHelper(*exec.Cmd)
	WriteAccessAdvice() string
	Relaunch(executable string, args []string) error
}

// Bundle operations are needed only on platforms that install an app bundle.
type bundleUpdater interface {
	AppBundle(binaryDir string) string
	ArchiveBinaryDir(root string) string
	MetadataDir(app string) string
	CheckInstall(dir string) error
	CheckDestination(dir, app string) error
	VerifyApp(app, cli, gui string) error
	InstallBundle(dir, app, stage string) error
}

func appBundle(dir string) string {
	if bundle, ok := native.(bundleUpdater); ok {
		return bundle.AppBundle(dir)
	}
	return ""
}

func checkInstallRestrictions(dir string) error {
	if bundle, ok := native.(bundleUpdater); ok {
		return bundle.CheckInstall(dir)
	}
	return nil
}
