package main

import (
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
	"time"
)

const (
	appName       = "Media Converter"
	preferredPort = 47391
)

// appVersion is set by build.sh from the VERSION file.
var appVersion = "1.1.0"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println(appVersion)
		return
	}
	cfgDir := configDir()
	_ = os.MkdirAll(cfgDir, 0o755)
	setupLog(cfgDir)

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
			if url, ok := existingInstance(cfgDir); ok {
				log.Printf("already running, opening %s", url)
				openBrowser(url)
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

func configDir() string {
	if d := os.Getenv("MC_CONFIG_DIR"); d != "" {
		return d
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "MediaConverter")
}

func setupLog(dir string) {
	f, err := os.Create(filepath.Join(dir, "log.txt"))
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

func existingInstance(dir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "instance.json"))
	if err != nil {
		return "", false
	}
	var info instanceInfo
	if json.Unmarshal(b, &info) != nil || info.Addr == "" {
		return "", false
	}
	req, _ := http.NewRequest("GET", "http://"+info.Addr+"/api/ping", nil)
	req.Header.Set("X-Token", info.Token)
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", false
	}
	return fmt.Sprintf("http://%s/?t=%s", info.Addr, info.Token), true
}
