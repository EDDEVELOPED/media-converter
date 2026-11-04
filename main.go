package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	appName       = "FFmpeg GUI - Media Converter"
	preferredPort = 47391
	// Settings folder. Changing it means people lose their settings and the
	// FFmpeg the app installed, unless the old folder is moved like below.
	configFolder    = "FFmpeg GUI - Media Converter"
	oldConfigFolder = "MediaConverter" // before 1.3.0, when the app was called Media Converter
)

// appVersion is set by build.sh from the VERSION file. Builds made without
// it say "dev" and never offer to replace themselves with a release.
var appVersion = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println(appVersion)
		return
	}
	cfgDir, movedFrom := configDir()
	_ = os.MkdirAll(cfgDir, 0o755)
	setupLog(cfgDir)
	if movedFrom != "" {
		log.Printf("moved settings from %s to %s", movedFrom, cfgDir)
	}

	// Files passed on the command line: "Open with", or dropped on the app's icon.
	var openFiles []string
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if st, err := os.Stat(arg); err == nil && !st.IsDir() {
			if abs, err := filepath.Abs(arg); err == nil {
				openFiles = append(openFiles, abs)
			}
		}
	}

	// After an update the new version takes over the old one's address and
	// token, so the open window reconnects by itself.
	restartAddr, restartToken := os.Getenv("MC_RESTART_ADDR"), os.Getenv("MC_RESTART_TOKEN")
	os.Unsetenv("MC_RESTART_ADDR")
	os.Unsetenv("MC_RESTART_TOKEN")
	var ln net.Listener
	var err error
	token := ""
	restarted := false
	if restartAddr != "" && restartToken != "" {
		for i := 0; i < 75; i++ {
			if ln, err = net.Listen("tcp", restartAddr); err == nil {
				token, restarted = restartToken, true
				log.Printf("restarted as version %s", appVersion)
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if ln == nil {
		// One copy at a time: if the app is already running, just bring its window up.
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", preferredPort))
		if err != nil {
			if url, info, ok := existingInstance(cfgDir); ok {
				log.Printf("already running, opening %s", url)
				if len(openFiles) > 0 {
					forwardFiles(info, openFiles)
				}
				if !focusAppWindow() { // its window was closed: open a new one
					openBrowser(url)
				}
				return
			}
			ln, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				fatal("Media Converter couldn't start.\n\n" + err.Error())
				return
			}
		}
		token = randomToken()
	}
	go cleanupOldBinary()

	addr := ln.Addr().String()
	url := fmt.Sprintf("http://%s/?t=%s", addr, token)
	writeInstance(cfgDir, addr, token)
	defer removeInstance(cfgDir)

	app := newApp(cfgDir, token, addr)
	defer app.cleanup()
	if len(openFiles) > 0 {
		go app.addPaths(openFiles, false)
	}

	srv := &http.Server{Handler: app.routes(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		<-app.quit
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	go app.watchIdle()

	if restarted {
		// The old window normally reconnects; open a new one if it doesn't.
		go func() {
			time.Sleep(12 * time.Second)
			if !app.hasConnected() && os.Getenv("MC_NO_BROWSER") == "" {
				openBrowser(url)
			}
		}()
	} else if os.Getenv("MC_NO_BROWSER") == "" {
		go func() {
			time.Sleep(200 * time.Millisecond)
			openBrowser(url)
		}()
	}
	log.Printf("%s %s listening on http://%s", appName, appVersion, addr)
	if os.Getenv("MC_PRINT_URL") != "" {
		fmt.Println(url)
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal("Media Converter stopped unexpectedly.\n\n" + err.Error())
	}
}

// configDir is where settings, the log and the FFmpeg the app installs are
// kept. The folder from before the rename is moved to the new name the first
// time it isn't in use; movedFrom is set when that just happened.
func configDir() (dir, movedFrom string) {
	if d := os.Getenv("MC_CONFIG_DIR"); d != "" {
		return d, ""
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	dir = filepath.Join(base, configFolder)
	old := filepath.Join(base, oldConfigFolder)
	if _, err := os.Stat(dir); err == nil {
		return dir, ""
	}
	if st, err := os.Stat(old); err != nil || !st.IsDir() {
		return dir, ""
	}
	// Right after an update the old version may still be closing, and Windows
	// can't rename a folder while a file in it is open, so wait for it.
	tries := 1
	if os.Getenv("MC_RESTART_ADDR") != "" {
		tries = 40
	}
	for i := 1; ; i++ {
		if err := os.Rename(old, dir); err == nil {
			fixMovedFFmpegPath(old, dir)
			return dir, old
		}
		if i >= tries {
			// Still in use, for example by an older copy that's open. Keep
			// using it as it is; it's moved on a later start.
			return old, ""
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// fixMovedFFmpegPath points a saved FFmpeg location that was inside the old
// settings folder (the copy the Install button put there) at the new folder.
func fixMovedFFmpegPath(old, dir string) {
	p := filepath.Join(dir, "settings.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	s := defaultSettings()
	if json.Unmarshal(data, &s) != nil || s.FFmpegPath == "" {
		return
	}
	rel, err := filepath.Rel(old, s.FFmpegPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}
	s.FFmpegPath = filepath.Join(dir, rel)
	if out, err := json.MarshalIndent(s, "", "  "); err == nil {
		_ = os.WriteFile(p, out, 0o644)
	}
}

func setupLog(dir string) {
	p := filepath.Join(dir, "log.txt")
	if st, err := os.Stat(p); err == nil && st.Size() > 2<<20 {
		_ = os.Remove(p) // start fresh once it gets big
	}
	// Append, so a second launch that just hands files to the running copy
	// doesn't wipe its log.
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	// File first: on Windows GUI builds stderr is not a valid handle.
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.SetFlags(log.LstdFlags)
}

func randomToken() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

type instanceInfo struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

func writeInstance(dir, addr, token string) {
	b, _ := json.Marshal(instanceInfo{Addr: addr, Token: token, PID: os.Getpid()})
	_ = os.WriteFile(filepath.Join(dir, "instance.json"), b, 0o600)
}

// removeInstance deletes instance.json only if it's still ours: after an
// update the new version has already written its own.
func removeInstance(dir string) {
	p := filepath.Join(dir, "instance.json")
	var info instanceInfo
	if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &info) == nil && info.PID == os.Getpid() {
		_ = os.Remove(p)
	}
}

func existingInstance(dir string) (string, instanceInfo, bool) {
	var info instanceInfo
	b, err := os.ReadFile(filepath.Join(dir, "instance.json"))
	if err != nil {
		return "", info, false
	}
	if json.Unmarshal(b, &info) != nil || info.Addr == "" {
		return "", info, false
	}
	req, _ := http.NewRequest("GET", "http://"+info.Addr+"/api/ping", nil)
	req.Header.Set("X-Token", info.Token)
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return "", info, false
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", info, false
	}
	return fmt.Sprintf("http://%s/?t=%s", info.Addr, info.Token), info, true
}

// forwardFiles hands files to the copy that's already running.
func forwardFiles(info instanceInfo, files []string) {
	body, _ := json.Marshal(map[string][]string{"paths": files})
	req, _ := http.NewRequest("POST", "http://"+info.Addr+"/api/add-paths", bytes.NewReader(body))
	req.Header.Set("X-Token", info.Token)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req); err == nil {
		resp.Body.Close()
	}
}
