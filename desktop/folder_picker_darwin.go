package desktop

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func pickCheckoutFolder(ctx context.Context, title, initial string) (string, error) {
	const script = `on run argv
	set promptText to item 1 of argv
	set startPath to item 2 of argv
	try
		activate
		if startPath is "" then
			set selectedFolder to choose folder with prompt promptText
		else
			set selectedFolder to choose folder with prompt promptText default location (POSIX file startPath as alias)
		end if
		return POSIX path of selectedFolder
	on error number -128
		return ""
	end try
end run`
	out, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", script, title, initial).Output()
	if err != nil {
		return "", fmt.Errorf("could not open the macOS folder picker: %w", err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}
