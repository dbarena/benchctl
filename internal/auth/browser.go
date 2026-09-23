package auth

import "os/exec"

// openBrowserCmd opens url in the system browser. Separated for OS portability.
func openBrowserCmd(url string) error {
	return exec.Command("open", url).Start()
}
