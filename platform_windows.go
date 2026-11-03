//go:build windows

package main

import (
	"encoding/base64"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

const createNoWindow = 0x08000000

// hideWindow stops a black console window flashing up for ffmpeg and PowerShell.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

func prepareBinary(path string) {}

func platformFFmpegPaths() []string {
	local := os.Getenv("LOCALAPPDATA")
	home := os.Getenv("USERPROFILE")
	var paths []string
	add := func(p ...string) { paths = append(paths, p...) }
	glob := func(pattern string) {
		if m, err := filepath.Glob(pattern); err == nil {
			add(m...)
		}
	}
	if local != "" {
		add(filepath.Join(local, `Microsoft\WinGet\Links\ffmpeg.exe`))
		glob(filepath.Join(local, `Microsoft\WinGet\Packages\*FFmpeg*\*\bin\ffmpeg.exe`))
		glob(filepath.Join(local, `Microsoft\WinGet\Packages\*FFmpeg*\bin\ffmpeg.exe`))
	}
	// PATH as saved in the registry, which can be newer than this process's
	// PATH right after installing FFmpeg.
	for _, key := range []string{`HKCU\Environment`, `HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Environment`} {
		for _, d := range regPathDirs(key) {
			add(filepath.Join(d, "ffmpeg.exe"))
		}
	}
	add(`C:\ffmpeg\bin\ffmpeg.exe`, `C:\ffmpeg\ffmpeg.exe`, `C:\ProgramData\chocolatey\bin\ffmpeg.exe`)
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		add(filepath.Join(pf, `ffmpeg\bin\ffmpeg.exe`))
	}
	if home != "" {
		add(filepath.Join(home, `scoop\shims\ffmpeg.exe`), filepath.Join(home, `scoop\apps\ffmpeg\current\bin\ffmpeg.exe`))
		glob(filepath.Join(home, `Downloads\ffmpeg*\bin\ffmpeg.exe`))
		glob(filepath.Join(home, `Downloads\ffmpeg*\ffmpeg*\bin\ffmpeg.exe`))
	}
	return paths
}

func regPathDirs(key string) []string {
	cmd := exec.Command("reg.exe", "query", key, "/v", "Path")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		i := strings.Index(line, "REG_")
		if i < 0 {
			continue
		}
		rest := line[i:]
		sp := strings.IndexAny(rest, " \t")
		if sp < 0 {
			continue
		}
		var dirs []string
		for _, d := range strings.Split(strings.TrimSpace(rest[sp:]), ";") {
			if d = strings.TrimSpace(expandWin(d)); d != "" {
				dirs = append(dirs, d)
			}
		}
		return dirs
	}
	return nil
}

func expandWin(s string) string {
	for n := 0; n < 20; n++ {
		i := strings.Index(s, "%")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i+1:], "%")
		if j < 0 {
			return s
		}
		s = s[:i] + os.Getenv(s[i+1:i+1+j]) + s[i+2+j:]
	}
	return s
}

// powershell runs a script with -EncodedCommand so no quoting can break it,
// and reads its output as UTF-8 so non-English file names survive.
func powershell(script string) (string, error) {
	full := "$ErrorActionPreference = 'Stop'\r\n[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false\r\n" + script
	u := utf16.Encode([]rune(full))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		b[2*i] = byte(v)
		b[2*i+1] = byte(v >> 8)
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", base64.StdEncoding.EncodeToString(b))
	hideWindow(cmd)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("The file picker didn't open: " + strings.TrimSpace(stderr.String()))
	}
	return strings.TrimPrefix(string(out), "\ufeff"), nil
}

const dialogPrelude = `Add-Type -AssemblyName System.Windows.Forms
[System.Windows.Forms.Application]::EnableVisualStyles()
$owner = New-Object System.Windows.Forms.Form
$owner.TopMost = $true
`

const mediaFilter = "Video and audio files|*.mp4;*.mov;*.m4v;*.mkv;*.avi;*.wmv;*.webm;*.flv;*.mpg;*.mpeg;*.ts;*.mts;*.m2ts;*.3gp;*.gif;*.mp3;*.wav;*.m4a;*.aac;*.flac;*.ogg;*.opus;*.wma;*.aif;*.aiff;*.amr|All files|*.*"

func lines(out string) []string {
	var res []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			res = append(res, l)
		}
	}
	return res
}

func pickFiles() ([]string, error) {
	out, err := powershell(dialogPrelude + `$d = New-Object System.Windows.Forms.OpenFileDialog
$d.Title = 'Choose videos or audio to convert'
$d.Multiselect = $true
$d.Filter = '` + mediaFilter + `'
if ($d.ShowDialog($owner) -eq [System.Windows.Forms.DialogResult]::OK) { foreach ($f in $d.FileNames) { [Console]::Out.WriteLine($f) } }
$owner.Dispose()
`)
	if err != nil {
		return nil, err
	}
	return lines(out), nil
}

func pickFolder() (string, error) {
	out, err := powershell(dialogPrelude + `$d = New-Object System.Windows.Forms.FolderBrowserDialog
$d.Description = 'Choose where to save converted files'
$d.ShowNewFolderButton = $true
if ($d.ShowDialog($owner) -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Out.WriteLine($d.SelectedPath) }
$owner.Dispose()
`)
	if err != nil {
		return "", err
	}
	if l := lines(out); len(l) > 0 {
		return l[0], nil
	}
	return "", nil
}

func pickExecutable() (string, error) {
	out, err := powershell(dialogPrelude + `$d = New-Object System.Windows.Forms.OpenFileDialog
$d.Title = 'Choose ffmpeg.exe (it is usually in the bin folder)'
$d.Filter = 'ffmpeg.exe|ffmpeg.exe|Programs|*.exe'
if ($d.ShowDialog($owner) -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Out.WriteLine($d.FileName) }
$owner.Dispose()
`)
	if err != nil {
		return "", err
	}
	if l := lines(out); len(l) > 0 {
		return l[0], nil
	}
	return "", nil
}

func reveal(path string) error {
	cmd := exec.Command("explorer.exe")
	// explorer needs /select,"path" exactly; set the raw command line.
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// openBrowser opens the interface in its own app-style window (Chrome or
// Edge, which every Windows PC has), falling back to the default browser.
func openBrowser(url string) {
	var cands []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
		if base := os.Getenv(env); base != "" {
			cands = append(cands, filepath.Join(base, `Google\Chrome\Application\chrome.exe`))
		}
	}
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
		if base := os.Getenv(env); base != "" {
			cands = append(cands, filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`))
		}
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err != nil {
			continue
		}
		cmd := exec.Command(c, "--app="+url, "--window-size=1120,820")
		if err := cmd.Start(); err == nil {
			go cmd.Wait()
			return
		}
	}
	cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url)
	if err := cmd.Start(); err != nil {
		log.Printf("couldn't open browser: %v", err)
		return
	}
	go cmd.Wait()
}

func fatal(msg string) {
	log.Print(msg)
	mb := syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")
	t, _ := syscall.UTF16PtrFromString(appName)
	m, _ := syscall.UTF16PtrFromString(msg)
	mb.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x10)
}

// detach starts the restarted app as its own process, not tied to this one.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
}
