//go:build !darwin && !windows

package main

import (
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
	return []string{"/usr/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/snap/bin/ffmpeg"}
}

func prepareBinary(path string) {
	if st, err := os.Stat(path); err == nil && st.Mode()&0o111 == 0 {
		_ = os.Chmod(path, 0o755)
	}
}

func zenity(args ...string) (string, error) {
	if _, err := exec.LookPath("zenity"); err != nil {
		return "", errors.New("No file picker is installed. Drag files into the window instead.")
	}
	out, err := exec.Command("zenity", args...).Output()
	if err != nil {
		return "", nil // canceled
	}
	return strings.TrimSpace(string(out)), nil
}

func pickFiles() ([]string, error) {
	out, err := zenity("--file-selection", "--multiple", "--separator=\n", "--title=Choose videos or audio to convert")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

func pickFolder() (string, error) {
	return zenity("--file-selection", "--directory", "--title=Choose where to save converted files")
}

func pickExecutable() (string, error) {
	return zenity("--file-selection", "--title=Choose the ffmpeg program file")
}

func reveal(path string) error {
	return exec.Command("xdg-open", filepath.Dir(path)).Start()
}

func openBrowser(url string) {
	if err := exec.Command("xdg-open", url).Start(); err != nil {
		log.Printf("couldn't open browser: %v", err)
	}
}

func fatal(msg string) { log.Print(msg) }

func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
