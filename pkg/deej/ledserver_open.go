package deej

import (
	"fmt"
	"os/exec"
	"runtime"
)

// openBrowser opens the given URL in the user's default browser
func openBrowser(url string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}

	// reap the helper process
	go func() { _ = cmd.Wait() }()

	return nil
}

// openCOMPortPicker opens the settings page so the user can choose a COM port.
// It returns false if the page couldn't be opened
func (d *Deej) openCOMPortPicker() bool {
	url, err := d.leds.server.ensureStarted()
	if err != nil {
		d.logger.Warnw("Failed to start settings server for COM port selection", "error", err)
		return false
	}

	if err := openBrowser(url); err != nil {
		d.logger.Warnw("Failed to open browser for COM port selection", "error", err, "url", url)
		return false
	}

	return true
}
