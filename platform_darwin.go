//go:build darwin

package main

import (
	"bytes"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func hideWindow(cmd *exec.Cmd) {}

func platformFFmpegPaths() []string {
	home, _ := os.UserHomeDir()
	return []string{
		"/opt/homebrew/bin/ffmpeg", // Homebrew on Apple Silicon
		"/usr/local/bin/ffmpeg",    // Homebrew on Intel, manual installs
		"/opt/local/bin/ffmpeg",    // MacPorts
		filepath.Join(home, "bin", "ffmpeg"),
		filepath.Join(home, "Downloads", "ffmpeg"),
		filepath.Join(home, "Applications", "ffmpeg"),
		"/Applications/ffmpeg",
	}
}

// prepareBinary makes a downloaded ffmpeg runnable: macOS blocks quarantined
// programs that aren't from the App Store or a known developer.
func prepareBinary(path string) {
	if st, err := os.Stat(path); err == nil && st.Mode()&0o111 == 0 {
		_ = os.Chmod(path, 0o755)
	}
	if exec.Command("xattr", "-p", "com.apple.quarantine", path).Run() == nil {
		_ = exec.Command("xattr", "-d", "com.apple.quarantine", path).Run()
	}
}

func osa(script string) (string, error) {
	cmd := exec.Command("osascript") // reads the script from stdin
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "-128") { // user pressed Cancel
			return "", nil
		}
		return "", errors.New("The file picker didn't open: " + strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

func pickFiles() ([]string, error) {
	out, err := osa(`activate
set picked to choose file with prompt "Choose videos or audio to convert" with multiple selections allowed
set out to ""
repeat with f in picked
	set out to out & POSIX path of f & linefeed
end repeat
return out`)
	if err != nil || out == "" {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\n") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

func pickFolder() (string, error) {
	out, err := osa(`activate
set f to choose folder with prompt "Choose where to save converted files"
return POSIX path of f`)
	if err != nil || out == "" {
		return "", err
	}
	return filepath.Clean(out), nil
}

func pickExecutable() (string, error) {
	out, err := osa(`activate
set f to choose file with prompt "Choose the ffmpeg program file"
return POSIX path of f`)
	if err != nil || out == "" {
		return "", err
	}
	return filepath.Clean(out), nil
}

func reveal(path string) error {
	return exec.Command("open", "-R", path).Run()
}

// openBrowser opens the interface in its own app-style window when a
// Chromium browser is installed, otherwise in a normal browser tab.
func openBrowser(url string) {
	home, _ := os.UserHomeDir()
	for _, name := range []string{"Google Chrome", "Microsoft Edge", "Brave Browser", "Chromium", "Vivaldi"} {
		for _, dir := range []string{"/Applications", filepath.Join(home, "Applications")} {
			app := filepath.Join(dir, name+".app")
			if _, err := os.Stat(app); err != nil {
				continue
			}
			cmd := exec.Command("open", "-na", app, "--args", "--app="+url, "--window-size=1120,820")
			if err := cmd.Run(); err == nil {
				return
			}
		}
	}
	if err := exec.Command("open", url).Run(); err != nil {
		log.Printf("couldn't open browser: %v", err)
	}
}

func fatal(msg string) {
	log.Print(msg)
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(msg)
	_, _ = osa(`display alert "` + appName + `" message "` + esc + `" as critical`)
}

// detach lets the restarted app outlive this process.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// focusAppWindow is only needed on Windows.
func focusAppWindow() bool { return false }
