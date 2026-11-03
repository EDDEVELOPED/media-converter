package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Where new versions of Media Converter are published.
const updateRepo = "EDDEVELOPED/media-converter"

// ---------- HTTP ----------

func userAgent() string { return "MediaConverter/" + appVersion + " (" + runtime.GOOS + ")" }

var smallClient = &http.Client{Timeout: 20 * time.Second}

var dlClient = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: t}
}()

var noRedirectClient = &http.Client{
	Timeout:       20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func friendlyNetErr(err error) error {
	var ne net.Error
	var dns *net.DNSError
	if errors.As(err, &dns) || errors.As(err, &ne) {
		return errors.New("couldn't reach the download server. Check your internet connection")
	}
	return err
}

func fetchText(ctx context.Context, u string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := smallClient.Do(req)
	if err != nil {
		return "", 0, friendlyNetErr(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return string(b), resp.StatusCode, err
}

var hex64 = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// fetchSHA reads a checksum file. It handles a bare hash and "hash  filename" lists.
func fetchSHA(ctx context.Context, u, fileName string) (string, error) {
	txt, code, err := fetchText(ctx, u)
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("checksum file returned %d", code)
	}
	var only string
	count := 0
	for _, line := range strings.Split(txt, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !hex64.MatchString(f[0]) {
			continue
		}
		count++
		only = f[0]
		if len(f) >= 2 && strings.TrimPrefix(f[len(f)-1], "*") == fileName {
			return strings.ToLower(f[0]), nil
		}
	}
	if count == 1 {
		return strings.ToLower(only), nil
	}
	return "", errors.New("no checksum found for " + fileName)
}

// download saves u to dest, reporting progress, and returns the file's SHA-256.
func download(ctx context.Context, u, dest string, progress func(done, total int64)) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := dlClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", friendlyNetErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("the download server answered %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	stall := time.AfterFunc(60*time.Second, cancel) // give up if nothing arrives for a minute
	defer stall.Stop()
	h := sha256.New()
	total := resp.ContentLength
	var done int64
	buf := make([]byte, 256<<10)
	last := time.Time{}
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			stall.Reset(60 * time.Second)
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(dest)
				return "", werr
			}
			h.Write(buf[:n])
			done += int64(n)
			if time.Since(last) > 200*time.Millisecond {
				progress(done, total)
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(dest)
			if ctx.Err() != nil {
				return "", errors.New("the download stopped")
			}
			return "", friendlyNetErr(rerr)
		}
	}
	progress(done, total)
	if err := f.Close(); err != nil {
		os.Remove(dest)
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------- version numbers ----------

func parseVer(s string) ([]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return nil, false
	}
	var out []int
	for _, p := range strings.Split(s, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// newerVersion reports whether a is a higher version than b.
func newerVersion(a, b string) bool {
	va, ok1 := parseVer(a)
	vb, ok2 := parseVer(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < len(va) || i < len(vb); i++ {
		x, y := 0, 0
		if i < len(va) {
			x = va[i]
		}
		if i < len(vb) {
			y = vb[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// ---------- FFmpeg download sources ----------

type ffBuild struct {
	Source      string    `json:"source"`
	Version     string    `json:"version"`
	BuildID     string    `json:"buildId"`
	URL         string    `json:"url"`
	SHAURL      string    `json:"-"`
	SHAName     string    `json:"-"`
	ShaRequired bool      `json:"-"`
	InstalledAt time.Time `json:"installedAt"`
}

type ffSource struct {
	ID     string
	Label  string
	SizeMB int
	latest func(ctx context.Context) (ffBuild, error)
}

var verRe = regexp.MustCompile(`^\d+(\.\d+)*$`)

func gyanSource() ffSource {
	const base = "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip"
	return ffSource{ID: "gyan", Label: "gyan.dev", SizeMB: 110, latest: func(ctx context.Context) (ffBuild, error) {
		v, code, err := fetchText(ctx, base+".ver")
		v = strings.TrimSpace(v)
		if err != nil {
			return ffBuild{}, err
		}
		if code != 200 || !verRe.MatchString(v) {
			return ffBuild{}, fmt.Errorf("couldn't read the latest version (%d)", code)
		}
		return ffBuild{Source: "gyan", Version: v, BuildID: "gyan-" + v, URL: base, SHAURL: base + ".sha256", ShaRequired: true}, nil
	}}
}

func btbnSource() ffSource {
	const dir = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/"
	const name = "ffmpeg-master-latest-win64-gpl.zip"
	return ffSource{ID: "btbn", Label: "BtbN on GitHub", SizeMB: 150, latest: func(ctx context.Context) (ffBuild, error) {
		sha, err := fetchSHA(ctx, dir+"checksums.sha256", name)
		if err != nil {
			return ffBuild{}, err
		}
		return ffBuild{Source: "btbn", BuildID: "btbn-" + sha, URL: dir + name, SHAURL: dir + "checksums.sha256", SHAName: name, ShaRequired: true}, nil
	}}
}

var riedlVerRe = regexp.MustCompile(`_(\d+(?:\.\d+)+)/`)

func riedlSource(arch string) ffSource {
	u := "https://ffmpeg.martin-riedl.de/redirect/latest/macos/" + arch + "/release/ffmpeg.zip"
	return ffSource{ID: "riedl-" + arch, Label: "ffmpeg.martin-riedl.de", SizeMB: 30, latest: func(ctx context.Context) (ffBuild, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		req.Header.Set("User-Agent", userAgent())
		resp, err := noRedirectClient.Do(req)
		if err != nil {
			return ffBuild{}, friendlyNetErr(err)
		}
		resp.Body.Close()
		loc := resp.Header.Get("Location")
		if resp.StatusCode < 300 || resp.StatusCode > 399 || loc == "" {
			return ffBuild{}, fmt.Errorf("unexpected answer %s", resp.Status)
		}
		if ref, err := url.Parse(loc); err == nil {
			loc = resp.Request.URL.ResolveReference(ref).String()
		}
		b := ffBuild{Source: "riedl-" + arch, BuildID: loc, URL: loc, SHAURL: loc + ".sha256", SHAName: "ffmpeg.zip"}
		if m := riedlVerRe.FindStringSubmatch(loc); m != nil {
			b.Version = m[1]
		}
		return b, nil
	}}
}

func evermeetSource() ffSource {
	return ffSource{ID: "evermeet", Label: "evermeet.cx", SizeMB: 26, latest: func(ctx context.Context) (ffBuild, error) {
		txt, code, err := fetchText(ctx, "https://evermeet.cx/ffmpeg/info/ffmpeg/release")
		if err != nil {
			return ffBuild{}, err
		}
		var info struct {
			Version  string `json:"version"`
			Download map[string]struct {
				URL string `json:"url"`
			} `json:"download"`
		}
		if code != 200 || json.Unmarshal([]byte(txt), &info) != nil || info.Download["zip"].URL == "" {
			return ffBuild{}, fmt.Errorf("couldn't read the latest version (%d)", code)
		}
		return ffBuild{Source: "evermeet", Version: info.Version, BuildID: "evermeet-" + info.Version, URL: info.Download["zip"].URL}, nil
	}}
}

func testSource(base string) ffSource {
	return ffSource{ID: "test", Label: "test server", SizeMB: 1, latest: func(ctx context.Context) (ffBuild, error) {
		v, code, err := fetchText(ctx, base+".ver")
		if err != nil || code != 200 {
			return ffBuild{}, fmt.Errorf("test source: %v %d", err, code)
		}
		v = strings.TrimSpace(v)
		return ffBuild{Source: "test", Version: v, BuildID: "test-" + v, URL: base, SHAURL: base + ".sha256", ShaRequired: true}, nil
	}}
}

func ffSources() []ffSource {
	if u := os.Getenv("MC_TEST_FFMPEG_URL"); u != "" {
		return []ffSource{testSource(u)}
	}
	switch runtime.GOOS {
	case "windows":
		return []ffSource{gyanSource(), btbnSource()} // x64 builds; Windows on ARM runs them too
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return []ffSource{riedlSource("arm64")}
		}
		return []ffSource{evermeetSource(), riedlSource("amd64")}
	}
	return nil
}

// ---------- installing FFmpeg ----------

type InstallState struct {
	Active bool   `json:"active"`
	Kind   string `json:"kind"`  // install | update
	Phase  string `json:"phase"` // checking | downloading | unpacking | testing | done | failed
	Done   int64  `json:"done"`
	Total  int64  `json:"total"`
	Source string `json:"source"`
	Error  string `json:"error"`
}

type FFUpdate struct {
	Version string `json:"version"`
	Current string `json:"current"`
}

func (a *App) managedFFmpeg() string {
	return filepath.Join(a.cfgDir, "ffmpeg", ffmpegName())
}

func (a *App) readInstalled() (ffBuild, bool) {
	var b ffBuild
	data, err := os.ReadFile(filepath.Join(a.cfgDir, "ffmpeg", "installed.json"))
	if err != nil || json.Unmarshal(data, &b) != nil {
		return b, false
	}
	return b, true
}

func (a *App) startFFInstall(kind string) error {
	if len(ffSources()) == 0 {
		return errors.New("Automatic install isn't available on this system.")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.install.Active {
		return errors.New("FFmpeg is already being installed.")
	}
	if a.running {
		return errors.New("Wait until converting finishes, then try again.")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.installCancel = cancel
	a.install = InstallState{Active: true, Kind: kind, Phase: "checking"}
	a.notify()
	go a.runFFInstall(ctx, kind)
	return nil
}

func (a *App) setInstall(f func(s *InstallState)) {
	a.mu.Lock()
	f(&a.install)
	a.notify()
	a.mu.Unlock()
}

func (a *App) runFFInstall(ctx context.Context, kind string) {
	var errs []string
	for _, src := range ffSources() {
		a.setInstall(func(s *InstallState) { s.Phase, s.Source, s.Done, s.Total = "checking", src.Label, 0, 0 })
		b, err := src.latest(ctx)
		if err == nil {
			err = a.installBuild(ctx, src, b)
		}
		if err == nil {
			a.setInstall(func(s *InstallState) { *s = InstallState{Kind: kind, Phase: "done", Source: src.Label} })
			a.mu.Lock()
			a.ffUpdate = nil
			a.mu.Unlock()
			a.refreshFFmpeg()
			return
		}
		if ctx.Err() != nil {
			a.setInstall(func(s *InstallState) { *s = InstallState{} })
			return
		}
		log.Printf("FFmpeg install from %s failed: %v", src.Label, err)
		errs = append(errs, src.Label+": "+err.Error())
	}
	msg := "FFmpeg couldn't be installed automatically. " + strings.Join(errs, "; ") + "."
	a.setInstall(func(s *InstallState) { *s = InstallState{Kind: kind, Phase: "failed", Error: msg} })
}

func (a *App) installBuild(ctx context.Context, src ffSource, b ffBuild) error {
	dir := filepath.Join(a.cfgDir, "ffmpeg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	expected := ""
	if b.SHAURL != "" {
		name := b.SHAName
		if name == "" {
			name = path.Base(b.URL)
		}
		sha, err := fetchSHA(ctx, b.SHAURL, name)
		if err != nil && b.ShaRequired {
			return fmt.Errorf("couldn't get the checksum: %v", err)
		}
		expected = sha
	}
	a.setInstall(func(s *InstallState) { s.Phase, s.Total = "downloading", int64(src.SizeMB)<<20 })
	zipPath := filepath.Join(dir, "download.zip")
	defer os.Remove(zipPath)
	got, err := download(ctx, b.URL, zipPath, func(done, total int64) {
		a.setInstall(func(s *InstallState) {
			s.Done = done
			if total > 0 {
				s.Total = total
			}
		})
	})
	if err != nil {
		return err
	}
	if expected != "" && !strings.EqualFold(expected, got) {
		return errors.New("the download was damaged (checksum didn't match)")
	}
	a.setInstall(func(s *InstallState) { s.Phase = "unpacking" })
	newPath := filepath.Join(dir, ffmpegName()+".new")
	if err := extractFFmpeg(zipPath, newPath); err != nil {
		return err
	}
	a.setInstall(func(s *InstallState) { s.Phase = "testing" })
	prepareBinary(newPath)
	info, err := checkFFmpeg(newPath)
	if err != nil {
		os.Remove(newPath)
		return fmt.Errorf("the downloaded FFmpeg didn't run: %v", err)
	}
	final := a.managedFFmpeg()
	var rerr error
	for i := 0; i < 20; i++ { // Windows can't replace a file that's still running
		if rerr = os.Rename(newPath, final); rerr == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if rerr != nil {
		os.Remove(newPath)
		return fmt.Errorf("couldn't put FFmpeg in place: %v", rerr)
	}
	if b.Version == "" {
		b.Version = info.Version
	}
	b.InstalledAt = time.Now()
	data, _ := json.MarshalIndent(b, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "installed.json"), data, 0o644)
	a.mu.Lock()
	a.settings.FFmpegPath = final
	a.saveSettingsLocked()
	a.mu.Unlock()
	log.Printf("installed FFmpeg %s from %s", b.Version, src.Label)
	return nil
}

func extractFFmpeg(zipPath, dest string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return errors.New("the download isn't a valid zip file")
	}
	defer zr.Close()
	var best *zip.File
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if f.FileInfo().IsDir() || strings.Contains(name, "__MACOSX") || path.Base(name) != ffmpegName() {
			continue
		}
		if best == nil || strings.Contains(name, "/bin/") {
			best = f
		}
	}
	if best == nil {
		return errors.New("the download didn't contain " + ffmpegName())
	}
	rc, err := best.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	return out.Close()
}

// checkFFUpdate looks for a newer FFmpeg, but only for the copy this app installed.
// Copies from Homebrew or winget are updated by those tools.
func (a *App) checkFFUpdate(ctx context.Context) {
	inst, ok := a.readInstalled()
	if !ok {
		return
	}
	a.mu.Lock()
	using := a.ff.Found && filepath.Clean(a.ff.Path) == filepath.Clean(a.managedFFmpeg())
	a.mu.Unlock()
	if !using {
		return
	}
	srcs := ffSources()
	var src *ffSource
	for i := range srcs {
		if srcs[i].ID == inst.Source {
			src = &srcs[i]
		}
	}
	if src == nil {
		if len(srcs) == 0 {
			return
		}
		src = &srcs[0]
	}
	b, err := src.latest(ctx)
	if err != nil {
		log.Printf("FFmpeg update check failed: %v", err)
		return
	}
	newer := false
	if b.Version != "" && inst.Version != "" && verRe.MatchString(b.Version) && verRe.MatchString(inst.Version) {
		newer = newerVersion(b.Version, inst.Version)
	} else {
		// Builds without version numbers: offer at most once a month.
		newer = b.BuildID != inst.BuildID && time.Since(inst.InstalledAt) > 30*24*time.Hour
	}
	if newer {
		v := b.Version
		if v == "" {
			v = "a newer build"
		}
		a.mu.Lock()
		a.ffUpdate = &FFUpdate{Version: v, Current: inst.Version}
		a.notify()
		a.mu.Unlock()
		log.Printf("FFmpeg update available: %s (have %s)", v, inst.Version)
	}
}

// ---------- updating the app itself ----------

type AppUpdate struct {
	Version string `json:"version"`
	Notes   string `json:"notes"`
	Page    string `json:"page"`
	Size    int64  `json:"size"`
	url     string
	sha     string
}

type UpdateProgress struct {
	Active bool   `json:"active"`
	Phase  string `json:"phase"` // downloading | installing | restarting
	Done   int64  `json:"done"`
	Total  int64  `json:"total"`
	Error  string `json:"error"`
}

func platformAsset() string {
	switch runtime.GOOS {
	case "windows":
		return "MediaConverter-windows-amd64.exe"
	case "darwin":
		return "MediaConverter-macos-universal"
	default:
		return "MediaConverter-" + runtime.GOOS + "-" + runtime.GOARCH
	}
}

func (a *App) checkAppUpdate(ctx context.Context) (bool, error) {
	api := os.Getenv("MC_UPDATE_API")
	if api == "" {
		api = "https://api.github.com/repos/" + updateRepo + "/releases/latest"
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", api, nil)
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := smallClient.Do(req)
	if err != nil {
		return false, friendlyNetErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return false, nil // nothing published yet
	}
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("update server answered %s", resp.Status)
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Body   string `json:"body"`
		Page   string `json:"html_url"`
		Draft  bool   `json:"draft"`
		Pre    bool   `json:"prerelease"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return false, err
	}
	ver := strings.TrimPrefix(rel.Tag, "v")
	if rel.Draft || rel.Pre || !newerVersion(ver, appVersion) {
		return false, nil
	}
	want := platformAsset()
	var assetURL, sumsURL string
	var size int64
	for _, as := range rel.Assets {
		switch as.Name {
		case want:
			assetURL, size = as.URL, as.Size
		case "checksums.txt":
			sumsURL = as.URL
		}
	}
	if assetURL == "" || sumsURL == "" {
		return false, nil // this release has nothing for this computer
	}
	sha, err := fetchSHA(ctx, sumsURL, want)
	if err != nil {
		return false, err
	}
	notes := strings.TrimSpace(rel.Body)
	if len(notes) > 600 {
		notes = notes[:600] + "…"
	}
	a.mu.Lock()
	a.appUpdate = &AppUpdate{Version: ver, Notes: notes, Page: rel.Page, Size: size, url: assetURL, sha: sha}
	a.notify()
	a.mu.Unlock()
	log.Printf("app update available: %s", ver)
	return true, nil
}

func (a *App) checkForUpdates() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	a.checkFFUpdate(ctx)
	return a.checkAppUpdate(ctx)
}

func (a *App) startAppUpdate() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.appUpdate == nil:
		return errors.New("There's no update to install.")
	case a.running:
		return errors.New("Wait until converting finishes, then update.")
	case a.updating.Active:
		return errors.New("The update is already being installed.")
	}
	a.updating = UpdateProgress{Active: true, Phase: "downloading", Total: a.appUpdate.Size}
	u := *a.appUpdate
	a.notify()
	go func() {
		if err := a.applyUpdate(u); err != nil {
			log.Printf("update failed: %v", err)
			a.mu.Lock()
			a.updating = UpdateProgress{Error: err.Error()}
			a.notify()
			a.mu.Unlock()
		}
	}()
	return nil
}

func (a *App) setUpdating(f func(u *UpdateProgress)) {
	a.mu.Lock()
	f(&a.updating)
	a.notify()
	a.mu.Unlock()
}

var plistVerRe = regexp.MustCompile(`(<key>CFBundle(?:ShortVersionString|Version)</key>\s*<string>)[^<]*(</string>)`)

func (a *App) applyUpdate(u AppUpdate) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	if runtime.GOOS == "darwin" && strings.Contains(exe, "/AppTranslocation/") {
		return errors.New("Move Media Converter into your Applications folder, open it from there, then update.")
	}
	newPath := exe + ".new"
	sha, err := download(context.Background(), u.url, newPath, func(done, total int64) {
		a.setUpdating(func(p *UpdateProgress) {
			p.Done = done
			if total > 0 {
				p.Total = total
			}
		})
	})
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return errors.New("Media Converter can't write to its own folder. Move it somewhere you own, like your Desktop or Applications, and try again.")
		}
		return errors.New("The update didn't download: " + err.Error() + ".")
	}
	if !strings.EqualFold(sha, u.sha) {
		os.Remove(newPath)
		return errors.New("The update was damaged while downloading. Try again later.")
	}
	a.setUpdating(func(p *UpdateProgress) { p.Phase = "installing" })
	_ = os.Chmod(newPath, 0o755)
	prepareBinary(newPath)

	// Make sure the new version actually runs before swapping it in.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	check := exec.CommandContext(ctx, newPath, "--version")
	hideWindow(check)
	out, err := check.Output()
	if err != nil || strings.TrimSpace(string(out)) != u.Version {
		os.Remove(newPath)
		return fmt.Errorf("The new version didn't start (%v). Your current version is unchanged.", err)
	}

	old := exe + ".old"
	_ = os.Remove(old)
	if runtime.GOOS == "windows" {
		// A running .exe can be renamed but not overwritten.
		if err := os.Rename(exe, old); err != nil {
			os.Remove(newPath)
			return errors.New("Couldn't replace the app. Move Media Converter.exe to a folder you own, like your Desktop, and try again.")
		}
		if err := os.Rename(newPath, exe); err != nil {
			_ = os.Rename(old, exe)
			os.Remove(newPath)
			return errors.New("Couldn't replace the app: " + err.Error())
		}
	} else if err := os.Rename(newPath, exe); err != nil {
		os.Remove(newPath)
		return errors.New("Couldn't replace the app: " + err.Error())
	}
	if runtime.GOOS == "darwin" {
		plist := filepath.Join(filepath.Dir(exe), "..", "Info.plist")
		if data, err := os.ReadFile(plist); err == nil {
			_ = os.WriteFile(plist, plistVerRe.ReplaceAll(data, []byte("${1}"+u.Version+"${2}")), 0o644)
		}
	}
	log.Printf("updated %s to %s, restarting", exe, u.Version)

	a.setUpdating(func(p *UpdateProgress) { p.Phase = "restarting" })
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "MC_RESTART_ADDR="+a.addr, "MC_RESTART_TOKEN="+a.token)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return errors.New("The update is installed, but the app couldn't restart. Open Media Converter again.")
	}
	go cmd.Process.Release()
	a.mu.Lock()
	a.restarting = true
	a.mu.Unlock()
	time.Sleep(300 * time.Millisecond) // let the window hear "restarting"
	a.requestQuit()
	return nil
}

// cleanupOldBinary removes the previous version left behind by an update.
func cleanupOldBinary() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	for i := 0; i < 40; i++ {
		err := os.Remove(exe + ".old")
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(500 * time.Millisecond) // the old copy may still be closing
	}
}
