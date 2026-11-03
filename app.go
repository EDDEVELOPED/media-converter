package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui/index.html
var uiFS embed.FS

type Settings struct {
	Format      string `json:"format"`
	Quality     string `json:"quality"`    // copy | high | balanced | small
	Resolution  string `json:"resolution"` // original | 1080 | 720 | 480
	RemoveAudio bool   `json:"removeAudio"`
	SaveMode    string `json:"saveMode"` // same | folder
	OutputDir   string `json:"outputDir"`
	FFmpegPath  string `json:"ffmpegPath"`
}

func defaultSettings() Settings {
	return Settings{Format: "mp4", Quality: "balanced", Resolution: "original", SaveMode: "same"}
}

type Job struct {
	ID           int      `json:"id"`
	Path         string   `json:"path"`
	Name         string   `json:"name"`
	Size         int64    `json:"size"`
	Uploaded     bool     `json:"uploaded"`
	Duration     float64  `json:"duration"`
	HasVideo     bool     `json:"hasVideo"`
	HasAudio     bool     `json:"hasAudio"`
	Width        int      `json:"width"`
	Height       int      `json:"height"`
	VideoCodec   string   `json:"videoCodec"`
	AudioCodec   string   `json:"audioCodec"`
	MultiChannel bool     `json:"-"`
	Container    string   `json:"container"`
	FPS          float64  `json:"fps"`
	InputArgs    []string `json:"-"`
	TrimStart    string   `json:"trimStart"`
	TrimEnd      string   `json:"trimEnd"`
	TrimError    string   `json:"trimError"`
	// probing | needs-ffmpeg | invalid | ready | queued | running | done | failed | canceled
	Status    string  `json:"status"`
	Progress  float64 `json:"progress"`
	Processed float64 `json:"processed"`
	Speed     string  `json:"speed"`
	ETA       float64 `json:"eta"`
	Output    string  `json:"output"`
	OutName   string  `json:"outName"`
	OutSize   int64   `json:"outSize"`
	Elapsed   float64 `json:"elapsed"`
	Error     string  `json:"error"`
	Detail    string  `json:"detail"`

	cancel context.CancelFunc
}

type App struct {
	mu        sync.Mutex
	cfgDir    string
	token     string
	addr      string
	settings  Settings
	ff        FFInfo
	checking  bool
	jobs      []*Job
	nextID    int
	running   bool
	clients   int
	connected bool
	lastLeft  time.Time
	started   time.Time
	subs      map[chan struct{}]bool
	quit      chan struct{}
	quitOnce  sync.Once
	uploadDir string
	dialogMu  sync.Mutex

	install       InstallState
	installCancel context.CancelFunc
	ffUpdate      *FFUpdate
	appUpdate     *AppUpdate
	updating      UpdateProgress
	restarting    bool
}

func (a *App) hasConnected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connected
}

func newApp(cfgDir, token, addr string) *App {
	a := &App{
		cfgDir:   cfgDir,
		token:    token,
		addr:     addr,
		settings: defaultSettings(),
		started:  time.Now(),
		subs:     map[chan struct{}]bool{},
		quit:     make(chan struct{}),
	}
	a.loadSettings()
	if d, err := os.MkdirTemp("", "mediaconverter-"); err == nil {
		a.uploadDir = d
	}
	a.checking = true
	go func() {
		a.refreshFFmpeg()
		if os.Getenv("MC_NO_UPDATE_CHECK") == "" {
			if _, err := a.checkForUpdates(); err != nil {
				log.Printf("update check failed: %v", err)
			}
		}
	}()
	return a
}

// ---------- settings ----------

func (a *App) loadSettings() {
	b, err := os.ReadFile(filepath.Join(a.cfgDir, "settings.json"))
	if err != nil {
		return
	}
	s := defaultSettings()
	if json.Unmarshal(b, &s) == nil {
		a.settings = normalize(s)
	}
}

func (a *App) saveSettingsLocked() {
	b, _ := json.MarshalIndent(a.settings, "", "  ")
	_ = os.WriteFile(filepath.Join(a.cfgDir, "settings.json"), b, 0o644)
}

func normalize(s Settings) Settings {
	f, ok := formatByID(s.Format)
	if !ok {
		s.Format, f = "mp4", formats[0]
	}
	switch s.Quality {
	case "high", "balanced", "small":
	case "copy":
		if !f.CanCopy {
			s.Quality = "balanced"
		}
	default:
		s.Quality = "balanced"
	}
	switch s.Resolution {
	case "original", "1080", "720", "480":
	default:
		s.Resolution = "original"
	}
	if s.SaveMode != "folder" || s.OutputDir == "" {
		s.SaveMode = "same"
	}
	return s
}

// ---------- state broadcast ----------

type State struct {
	Jobs      []*Job       `json:"jobs"`
	Settings  Settings     `json:"settings"`
	FFmpeg    FFInfo       `json:"ffmpeg"`
	Checking  bool         `json:"checking"`
	Formats   []FormatView `json:"formats"`
	Running   bool         `json:"running"`
	OS        string       `json:"os"`
	Downloads string       `json:"downloads"`
	Version   string       `json:"version"`

	Install     InstallState   `json:"install"`
	CanInstall  bool           `json:"canInstall"`
	InstallFrom string         `json:"installFrom"`
	InstallMB   int            `json:"installMB"`
	FFUpdate    *FFUpdate      `json:"ffUpdate"`
	AppUpdate   *AppUpdate     `json:"appUpdate"`
	Updating    UpdateProgress `json:"updating"`
}

func (a *App) notify() {
	for ch := range a.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (a *App) stateJSON() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := State{
		Jobs:      a.jobs,
		Settings:  a.settings,
		FFmpeg:    a.ff,
		Checking:  a.checking,
		Formats:   a.ff.formatViews(),
		Running:   a.running,
		OS:        runtime.GOOS,
		Downloads: downloadsDir(),
		Version:   appVersion,
		Install:   a.install,
		FFUpdate:  a.ffUpdate,
		AppUpdate: a.appUpdate,
		Updating:  a.updating,
	}
	if srcs := ffSources(); len(srcs) > 0 {
		st.CanInstall, st.InstallFrom, st.InstallMB = true, srcs[0].Label, srcs[0].SizeMB
	}
	if st.Jobs == nil {
		st.Jobs = []*Job{}
	}
	b, _ := json.Marshal(st)
	return b
}

// ---------- FFmpeg ----------

func (a *App) refreshFFmpeg() FFInfo {
	a.mu.Lock()
	manual := a.settings.FFmpegPath
	a.checking = true
	a.notify()
	a.mu.Unlock()

	info := detectFFmpeg(manual, a.managedFFmpeg())
	if info.Found {
		log.Printf("using FFmpeg %s at %s", info.Version, info.Path)
	} else {
		log.Printf("FFmpeg not found: %s", info.Error)
	}

	a.mu.Lock()
	a.ff = info
	a.checking = false
	var waiting []*Job
	if info.Found {
		for _, j := range a.jobs {
			if j.Status == "needs-ffmpeg" {
				j.Status = "probing"
				waiting = append(waiting, j)
			}
		}
	}
	a.notify()
	a.mu.Unlock()
	for _, j := range waiting {
		go a.probeJob(j)
	}
	return info
}

// ---------- jobs ----------

func (a *App) addPaths(paths []string, uploaded bool) int {
	added := 0
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			if abs, err := filepath.Abs(p); err == nil {
				p = abs
			}
		}
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		a.mu.Lock()
		dup := false
		if !uploaded {
			for _, j := range a.jobs {
				if j.Path == p && j.Status != "done" {
					dup = true
					break
				}
			}
		}
		if dup {
			a.mu.Unlock()
			continue
		}
		a.nextID++
		j := &Job{ID: a.nextID, Path: p, Name: filepath.Base(p), Size: st.Size(), Uploaded: uploaded, Status: "probing"}
		if !a.ff.Found {
			if a.checking {
				j.Status = "probing"
			} else {
				j.Status = "needs-ffmpeg"
			}
		}
		a.jobs = append(a.jobs, j)
		probeNow := a.ff.Found
		a.notify()
		a.mu.Unlock()
		if probeNow {
			go a.probeJob(j)
		} else {
			go a.waitThenProbe(j)
		}
		added++
	}
	return added
}

// waitThenProbe covers files added while FFmpeg detection is still running.
func (a *App) waitThenProbe(j *Job) {
	for i := 0; i < 200; i++ {
		a.mu.Lock()
		checking, found := a.checking, a.ff.Found
		if !checking {
			if !found && j.Status == "probing" {
				j.Status = "needs-ffmpeg"
				a.notify()
			}
			a.mu.Unlock()
			if found {
				a.probeJob(j)
			}
			return
		}
		a.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
}

func (a *App) probeJob(j *Job) {
	a.mu.Lock()
	ff := a.ff.Path
	a.mu.Unlock()
	info, inArgs, err := probe(ff, j.Path)
	a.mu.Lock()
	defer a.mu.Unlock()
	if j.Status != "probing" {
		return
	}
	if err != nil {
		j.Status = "invalid"
		j.Error = err.Error()
	} else {
		j.Duration, j.HasVideo, j.HasAudio = info.Duration, info.HasVideo, info.HasAudio
		j.Width, j.Height = info.Width, info.Height
		j.VideoCodec, j.AudioCodec, j.MultiChannel = info.VideoCodec, info.AudioCodec, info.MultiChannel
		j.Container, j.FPS, j.InputArgs = info.Container, info.FPS, inArgs
		j.Status = "ready"
	}
	a.notify()
}

func (a *App) jobByID(id int) *Job {
	for _, j := range a.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (j *Job) trimValues() (start, end float64, msg string) {
	var err error
	if start, err = parseTime(j.TrimStart); err != nil {
		return 0, 0, "Start time isn't valid. Use a format like 1:30 or 90."
	}
	if end, err = parseTime(j.TrimEnd); err != nil {
		return 0, 0, "End time isn't valid. Use a format like 2:45 or 165."
	}
	if end > 0 && end <= start {
		return 0, 0, "End time needs to be after the start time."
	}
	if j.Duration > 0 && start >= j.Duration {
		return 0, 0, "Start time is past the end of the file."
	}
	if j.Duration > 0 && end > j.Duration+0.5 {
		end = 0 // past the end: just convert to the end
	}
	return start, end, ""
}

func (a *App) removeUpload(j *Job) {
	if j.Uploaded && a.uploadDir != "" && strings.HasPrefix(j.Path, a.uploadDir) {
		_ = os.RemoveAll(filepath.Dir(j.Path))
	}
}

// ---------- the queue ----------

func (a *App) runQueue() {
	for {
		a.mu.Lock()
		var j *Job
		for _, x := range a.jobs {
			if x.Status == "queued" {
				j = x
				break
			}
		}
		if j == nil {
			a.running = false
			a.notify()
			a.mu.Unlock()
			return
		}
		s := a.settings
		ffPath := a.ff.Path
		ctx, cancel := context.WithCancel(context.Background())
		j.Status, j.Progress, j.Processed, j.Speed, j.ETA = "running", 0, 0, "", 0
		j.Error, j.Detail, j.Output, j.OutName, j.OutSize = "", "", "", "", 0
		j.cancel = cancel
		a.notify()
		a.mu.Unlock()

		a.convert(ctx, j, s, ffPath)
		cancel()
	}
}

type tailBuf struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 16000 {
		t.buf = t.buf[len(t.buf)-12000:]
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func (a *App) convert(ctx context.Context, j *Job, s Settings, ffPath string) {
	finish := func(status, msg, detail string) {
		a.mu.Lock()
		j.Status, j.Error, j.Detail, j.cancel = status, msg, detail, nil
		a.notify()
		a.mu.Unlock()
	}
	f, ok := formatByID(s.Format)
	if !ok {
		finish("failed", "Unknown format.", "")
		return
	}
	if f.Group == "video" && !j.HasVideo {
		finish("failed", "This file has no video. Pick an audio format such as MP3 or M4A.", "")
		return
	}
	if f.Group == "audio" && !j.HasAudio {
		finish("failed", "This file has no sound to save.", "")
		return
	}
	start, end, msg := j.trimValues()
	if msg != "" {
		finish("failed", msg, "")
		return
	}
	if _, err := os.Stat(j.Path); err != nil {
		finish("failed", "The original file can't be found. It may have been moved or renamed.", "")
		return
	}
	out, err := outputPath(j, s, f)
	if err != nil {
		finish("failed", err.Error(), "")
		return
	}
	dur := 0.0
	if end > 0 {
		dur = end - start
	} else if j.Duration > 0 {
		dur = j.Duration - start
	}
	args := buildArgs(j, s, f, out, start, end > 0, dur)
	log.Printf("job %d: %s %q", j.ID, ffPath, args)

	cmd := exec.CommandContext(ctx, ffPath, args...)
	hideWindow(cmd)
	cmd.WaitDelay = 5 * time.Second
	var stderr tailBuf
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = os.Remove(out)
		finish("failed", "FFmpeg couldn't be started.", err.Error())
		return
	}
	a.mu.Lock()
	j.Output, j.OutName = out, filepath.Base(out)
	a.mu.Unlock()

	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		_ = os.Remove(out)
		finish("failed", "FFmpeg couldn't be started.", err.Error())
		return
	}
	sc := bufio.NewScanner(stdout)
	var outUs int64
	speed := ""
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "out_time_us":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
				outUs = n
			}
		case "speed":
			speed = v
		case "progress":
			t := float64(outUs) / 1e6
			el := time.Since(t0).Seconds()
			a.mu.Lock()
			j.Processed = t
			if dur > 0 {
				p := t / dur
				if p > 0.999 {
					p = 0.999
				}
				if p < 0 {
					p = 0
				}
				j.Progress = p
				if p > 0.01 && el > 1.5 {
					j.ETA = el * (1 - p) / p
				}
			}
			if speed != "" && speed != "N/A" {
				j.Speed = speed
			}
			a.notify()
			a.mu.Unlock()
		}
	}
	err = cmd.Wait()
	el := time.Since(t0).Seconds()

	a.mu.Lock()
	defer a.mu.Unlock()
	j.cancel = nil
	j.Elapsed = el
	if ctx.Err() != nil {
		_ = os.Remove(out)
		j.Status, j.Output, j.OutName, j.Progress = "canceled", "", "", 0
		a.notify()
		return
	}
	if err != nil {
		_ = os.Remove(out)
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		j.Status, j.Error, j.Detail, j.Output, j.OutName = "failed", friendlyError(detail, s, f), detail, "", ""
		log.Printf("job %d failed: %s", j.ID, detail)
		a.notify()
		return
	}
	if st, err := os.Stat(out); err == nil {
		j.OutSize = st.Size()
	}
	j.Status, j.Progress, j.ETA = "done", 1, 0
	a.notify()
}

// outputPath picks a name that doesn't exist yet and reserves it, so nothing
// is ever overwritten.
func outputPath(j *Job, s Settings, f Format) (string, error) {
	dir := filepath.Dir(j.Path)
	if s.SaveMode == "folder" && s.OutputDir != "" {
		dir = s.OutputDir
	} else if j.Uploaded {
		dir = downloadsDir()
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("The save folder doesn't exist: %s", dir)
	}
	base := strings.TrimSuffix(j.Name, filepath.Ext(j.Name))
	for i := 0; i < 500; i++ {
		name := base + "." + f.Ext
		if i == 1 {
			name = base + " (converted)." + f.Ext
		} else if i > 1 {
			name = fmt.Sprintf("%s (converted %d).%s", base, i, f.Ext)
		}
		p := filepath.Join(dir, name)
		fh, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			fh.Close()
			return p, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if errors.Is(err, os.ErrPermission) {
			return "", errors.New("Media Converter can't save to " + dir + ". Pick another folder under Save to.")
		}
		return "", fmt.Errorf("Couldn't create the new file: %v", err)
	}
	return "", errors.New("Couldn't find a free file name in the save folder.")
}

func downloadsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir()
	}
	d := filepath.Join(home, "Downloads")
	if st, err := os.Stat(d); err == nil && st.IsDir() {
		return d
	}
	return home
}

// ---------- lifetime ----------

func (a *App) requestQuit() {
	a.quitOnce.Do(func() { close(a.quit) })
}

// watchIdle closes the app once its window has been closed and nothing is converting.
func (a *App) watchIdle() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.quit:
			return
		case <-t.C:
		}
		if os.Getenv("MC_STAY") != "" {
			continue
		}
		a.mu.Lock()
		exit := false
		if a.clients == 0 && !a.running {
			if !a.connected && time.Since(a.started) > 3*time.Minute {
				exit = true
			}
			if a.connected && time.Since(a.lastLeft) > 30*time.Second {
				exit = true
			}
		}
		a.mu.Unlock()
		if exit {
			log.Print("window closed, exiting")
			a.requestQuit()
			return
		}
	}
}

func (a *App) cleanup() {
	a.mu.Lock()
	for _, j := range a.jobs {
		if j.Status == "queued" {
			j.Status = "ready"
		}
		if j.cancel != nil {
			j.cancel()
		}
	}
	a.mu.Unlock()
	for i := 0; i < 60; i++ {
		a.mu.Lock()
		r := a.running
		a.mu.Unlock()
		if !r {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if a.uploadDir != "" {
		_ = os.RemoveAll(a.uploadDir)
	}
}

// ---------- HTTP ----------

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	mux.HandleFunc("GET /api/events", a.handleEvents)
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(a.stateJSON())
	})
	mux.HandleFunc("POST /api/add-files", a.handleAddFiles)
	mux.HandleFunc("POST /api/add-paths", a.handleAddPaths)
	mux.HandleFunc("POST /api/upload", a.handleUpload)
	mux.HandleFunc("POST /api/settings", a.handleSettings)
	mux.HandleFunc("POST /api/pick-folder", a.handlePickFolder)
	mux.HandleFunc("POST /api/job", a.handleJobUpdate)
	mux.HandleFunc("POST /api/remove", a.handleRemove)
	mux.HandleFunc("POST /api/clear", a.handleClear)
	mux.HandleFunc("POST /api/retry", a.handleRetry)
	mux.HandleFunc("POST /api/start", a.handleStart)
	mux.HandleFunc("POST /api/stop", a.handleStop)
	mux.HandleFunc("POST /api/cancel", a.handleCancel)
	mux.HandleFunc("POST /api/reveal", a.handleReveal)
	mux.HandleFunc("POST /api/ffmpeg/check", a.handleFFCheck)
	mux.HandleFunc("POST /api/ffmpeg/locate", a.handleFFLocate)
	mux.HandleFunc("POST /api/ffmpeg/install", func(w http.ResponseWriter, r *http.Request) { a.handleInstall(w, "install") })
	mux.HandleFunc("POST /api/ffmpeg/update", func(w http.ResponseWriter, r *http.Request) { a.handleInstall(w, "update") })
	mux.HandleFunc("POST /api/ffmpeg/cancel", a.handleInstallCancel)
	mux.HandleFunc("POST /api/update/check", a.handleUpdateCheck)
	mux.HandleFunc("POST /api/update/apply", a.handleUpdateApply)
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
		go func() { time.Sleep(150 * time.Millisecond); a.requestQuit() }()
	})
	return a.guard(mux)
}

// guard only lets this app's own window talk to it: the Host must be our
// loopback address and API calls must carry the startup token.
func (a *App) guard(next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(a.addr)
	okHosts := map[string]bool{a.addr: true, "localhost:" + port: true, "127.0.0.1:" + port: true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !okHosts[r.Host] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			t := r.Header.Get("X-Token")
			if t == "" && r.URL.Path == "/api/events" {
				t = r.URL.Query().Get("t")
			}
			if subtle.ConstantTimeCompare([]byte(t), []byte(a.token)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Open Media Converter from its icon."})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	b, _ := uiFS.ReadFile("ui/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
	w.Write(b)
}

func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	fmt.Fprint(w, "retry: 1000\n\n")
	ch := make(chan struct{}, 1)
	a.mu.Lock()
	a.subs[ch] = true
	a.clients++
	a.connected = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.subs, ch)
		a.clients--
		a.lastLeft = time.Now()
		a.mu.Unlock()
	}()
	send := func() bool {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", a.stateJSON()); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send() {
		return
	}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.quit:
			a.mu.Lock()
			restarting := a.restarting
			a.mu.Unlock()
			fmt.Fprintf(w, "event: quit\ndata: {\"restart\":%v}\n\n", restarting)
			fl.Flush()
			return
		case <-ch:
			time.Sleep(60 * time.Millisecond) // let bursts of changes settle
			select {
			case <-ch:
			default:
			}
			if !send() {
				return
			}
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func (a *App) handleAddFiles(w http.ResponseWriter, r *http.Request) {
	if !a.dialogMu.TryLock() {
		fail(w, http.StatusConflict, "A file picker is already open.")
		return
	}
	defer a.dialogMu.Unlock()
	paths, err := pickFiles()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	n := a.addPaths(paths, false)
	writeJSON(w, 200, map[string]any{"added": n})
}

func (a *App) handleAddPaths(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := readBody(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	writeJSON(w, 200, map[string]any{"added": a.addPaths(body.Paths, false)})
}

func safeName(n string) string {
	n = filepath.Base(strings.ReplaceAll(n, "\\", "/"))
	n = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, n)
	n = strings.Trim(n, ". ")
	if n == "" {
		n = "file"
	}
	return n
}

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	if a.uploadDir == "" {
		fail(w, 500, "No temporary folder available.")
		return
	}
	name := safeName(r.URL.Query().Get("name"))
	dir, err := os.MkdirTemp(a.uploadDir, "f")
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	_, err = io.Copy(f, r.Body)
	f.Close()
	if err != nil {
		_ = os.RemoveAll(dir)
		fail(w, 500, "The file didn't finish copying.")
		return
	}
	n := a.addPaths([]string{p}, true)
	if n == 0 {
		_ = os.RemoveAll(dir)
	}
	writeJSON(w, 200, map[string]any{"added": n})
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	var s Settings
	if err := readBody(r, &s); err != nil {
		fail(w, 400, "bad request")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		fail(w, http.StatusConflict, "Settings can't change while converting.")
		return
	}
	s.OutputDir = a.settings.OutputDir
	s.FFmpegPath = a.settings.FFmpegPath
	a.settings = normalize(s)
	a.saveSettingsLocked()
	a.notify()
	writeJSON(w, 200, a.settings)
}

func (a *App) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	if !a.dialogMu.TryLock() {
		fail(w, http.StatusConflict, "A file picker is already open.")
		return
	}
	defer a.dialogMu.Unlock()
	dir, err := pickFolder()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if dir != "" {
		a.settings.OutputDir = dir
		a.settings.SaveMode = "folder"
		a.saveSettingsLocked()
		a.notify()
	}
	writeJSON(w, 200, map[string]any{"dir": dir})
}

type idBody struct {
	ID        int     `json:"id"`
	TrimStart *string `json:"trimStart"`
	TrimEnd   *string `json:"trimEnd"`
}

func (a *App) handleJobUpdate(w http.ResponseWriter, r *http.Request) {
	var b idBody
	if err := readBody(r, &b); err != nil {
		fail(w, 400, "bad request")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	j := a.jobByID(b.ID)
	if j == nil {
		fail(w, 404, "not found")
		return
	}
	if j.Status == "running" || j.Status == "queued" {
		fail(w, http.StatusConflict, "This file is converting.")
		return
	}
	if b.TrimStart != nil {
		j.TrimStart = strings.TrimSpace(*b.TrimStart)
	}
	if b.TrimEnd != nil {
		j.TrimEnd = strings.TrimSpace(*b.TrimEnd)
	}
	_, _, j.TrimError = j.trimValues()
	a.notify()
	writeJSON(w, 200, map[string]any{"trimError": j.TrimError})
}

func (a *App) handleRemove(w http.ResponseWriter, r *http.Request) {
	var b idBody
	if err := readBody(r, &b); err != nil {
		fail(w, 400, "bad request")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, j := range a.jobs {
		if j.ID == b.ID {
			if j.Status == "running" {
				fail(w, http.StatusConflict, "Stop this file before removing it.")
				return
			}
			a.removeUpload(j)
			a.jobs = append(a.jobs[:i], a.jobs[i+1:]...)
			a.notify()
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	fail(w, 404, "not found")
}

func (a *App) handleClear(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	keep := a.jobs[:0]
	for _, j := range a.jobs {
		switch j.Status {
		case "done", "invalid", "canceled", "failed":
			a.removeUpload(j)
		default:
			keep = append(keep, j)
		}
	}
	a.jobs = keep
	a.notify()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleRetry(w http.ResponseWriter, r *http.Request) {
	var b idBody
	if err := readBody(r, &b); err != nil {
		fail(w, 400, "bad request")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	j := a.jobByID(b.ID)
	if j == nil {
		fail(w, 404, "not found")
		return
	}
	switch j.Status {
	case "done", "failed", "canceled":
		j.Status, j.Progress, j.Error, j.Detail, j.ETA, j.Speed = "ready", 0, "", "", 0, ""
		j.Output, j.OutName, j.OutSize = "", "", 0
		if a.running {
			j.Status = "queued"
		}
	}
	a.notify()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleStart(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ff.Found {
		fail(w, 400, "FFmpeg isn't set up yet.")
		return
	}
	if a.install.Active || a.updating.Active {
		fail(w, http.StatusConflict, "Wait for the update to finish.")
		return
	}
	for _, f := range a.ff.formatViews() {
		if f.ID == a.settings.Format && !f.Available {
			fail(w, 400, "Your copy of FFmpeg can't make "+f.Label+" files. Pick another format.")
			return
		}
	}
	n := 0
	for _, j := range a.jobs {
		if j.Status == "ready" {
			j.Status = "queued"
			n++
		}
	}
	if n == 0 {
		fail(w, 400, "There are no files ready to convert.")
		return
	}
	if !a.running {
		a.running = true
		go a.runQueue()
	}
	a.notify()
	writeJSON(w, 200, map[string]any{"queued": n})
}

func (a *App) handleStop(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, j := range a.jobs {
		if j.Status == "queued" {
			j.Status = "ready"
		}
		if j.cancel != nil {
			j.cancel()
		}
	}
	a.notify()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleCancel(w http.ResponseWriter, r *http.Request) {
	var b idBody
	if err := readBody(r, &b); err != nil {
		fail(w, 400, "bad request")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	j := a.jobByID(b.ID)
	if j == nil {
		fail(w, 404, "not found")
		return
	}
	if j.Status == "queued" {
		j.Status = "ready"
	}
	if j.cancel != nil {
		j.cancel()
	}
	a.notify()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleReveal(w http.ResponseWriter, r *http.Request) {
	var b idBody
	if err := readBody(r, &b); err != nil {
		fail(w, 400, "bad request")
		return
	}
	a.mu.Lock()
	j := a.jobByID(b.ID)
	var p string
	if j != nil {
		p = j.Output
	}
	a.mu.Unlock()
	if p == "" {
		fail(w, 404, "not found")
		return
	}
	if _, err := os.Stat(p); err != nil {
		fail(w, 404, "The converted file has been moved or deleted.")
		return
	}
	if err := reveal(p); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleFFCheck(w http.ResponseWriter, r *http.Request) {
	info := a.refreshFFmpeg()
	writeJSON(w, 200, map[string]any{"found": info.Found})
}

func (a *App) handleFFLocate(w http.ResponseWriter, r *http.Request) {
	if !a.dialogMu.TryLock() {
		fail(w, http.StatusConflict, "A file picker is already open.")
		return
	}
	p, err := pickExecutable()
	a.dialogMu.Unlock()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if p == "" {
		writeJSON(w, 200, map[string]any{"found": false, "canceled": true})
		return
	}
	prepareBinary(p)
	if _, err := checkFFmpeg(p); err != nil {
		fail(w, 400, "That file isn't a working copy of FFmpeg. Choose the file named "+ffmpegName()+".")
		return
	}
	a.mu.Lock()
	a.settings.FFmpegPath = p
	a.saveSettingsLocked()
	a.mu.Unlock()
	info := a.refreshFFmpeg()
	writeJSON(w, 200, map[string]any{"found": info.Found})
}

func (a *App) handleInstall(w http.ResponseWriter, kind string) {
	if err := a.startFFInstall(kind); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleInstallCancel(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	if a.install.Active && a.installCancel != nil {
		a.installCancel()
	}
	a.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	found, err := a.checkForUpdates()
	a.mu.Lock()
	ff := a.ffUpdate != nil
	a.mu.Unlock()
	if err != nil {
		fail(w, 502, "Couldn't check for updates: "+err.Error()+".")
		return
	}
	writeJSON(w, 200, map[string]any{"app": found, "ffmpeg": ff})
}

func (a *App) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if err := a.startAppUpdate(); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
