package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ---------- Formats ----------

type Format struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Group    string   `json:"group"` // video | audio
	Ext      string   `json:"ext"`
	Desc     string   `json:"desc"`
	Needs    []string `json:"-"`
	CanCopy  bool     `json:"canCopy"`
	Lossless bool     `json:"lossless"`
}

type FormatView struct {
	Format
	Available bool   `json:"available"`
	Missing   string `json:"missing,omitempty"`
}

var formats = []Format{
	{ID: "mp4", Label: "MP4", Group: "video", Ext: "mp4", Needs: []string{"libx264", "aac"}, CanCopy: true,
		Desc: "H.264 video. Plays on phones, browsers, TVs and editing apps."},
	{ID: "hevc", Label: "MP4 (HEVC)", Group: "video", Ext: "mp4", Needs: []string{"libx265", "aac"},
		Desc: "H.265 video. About half the size of regular MP4 at the same quality, but older devices may not play it."},
	{ID: "webm", Label: "WebM", Group: "video", Ext: "webm", Needs: []string{"libvpx-vp9", "libopus"},
		Desc: "VP9 video. Small files for websites. Converting is slower than MP4."},
	{ID: "mov", Label: "MOV", Group: "video", Ext: "mov", Needs: []string{"libx264", "aac"}, CanCopy: true,
		Desc: "H.264 in a QuickTime file. Good for Final Cut, iMovie and other Mac apps."},
	{ID: "mkv", Label: "MKV", Group: "video", Ext: "mkv", Needs: []string{"libx264", "aac"}, CanCopy: true,
		Desc: "H.264 in a Matroska file. Good for keeping several audio or subtitle tracks."},
	{ID: "gif", Label: "GIF", Group: "video", Ext: "gif", Needs: []string{"gif"},
		Desc: "Animated image with no sound. Best for clips under 15 seconds, since long GIFs get very large."},
	{ID: "mp3", Label: "MP3", Group: "audio", Ext: "mp3", Needs: []string{"libmp3lame"},
		Desc: "Plays on everything."},
	{ID: "m4a", Label: "M4A", Group: "audio", Ext: "m4a", Needs: []string{"aac"},
		Desc: "AAC audio. Smaller than MP3 at the same quality, and works well with Apple devices."},
	{ID: "wav", Label: "WAV", Group: "audio", Ext: "wav", Needs: []string{"pcm_s16le"}, Lossless: true,
		Desc: "Uncompressed audio. Large files, best for editing."},
	{ID: "flac", Label: "FLAC", Group: "audio", Ext: "flac", Needs: []string{"flac"}, Lossless: true,
		Desc: "Lossless audio at about half the size of WAV."},
	{ID: "ogg", Label: "OGG", Group: "audio", Ext: "ogg", Needs: []string{"libopus"},
		Desc: "Opus audio. Very small files that still sound good."},
}

func formatByID(id string) (Format, bool) {
	for _, f := range formats {
		if f.ID == id {
			return f, true
		}
	}
	return Format{}, false
}

// ---------- Finding FFmpeg ----------

type FFInfo struct {
	Found    bool            `json:"found"`
	Path     string          `json:"path"`
	Version  string          `json:"version"`
	Error    string          `json:"error,omitempty"`
	encoders map[string]bool `json:"-"`
}

func ffmpegName() string {
	if runtime.GOOS == "windows" {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
}

func detectFFmpeg(manual, managed string) FFInfo {
	name := ffmpegName()
	var cands []string
	if manual != "" {
		cands = append(cands, manual)
	}
	cands = append(cands, managed) // the copy installed by the Install button
	// MC_TEST_NO_FFMPEG pretends nothing else is installed (for testing the installer).
	systemWide := os.Getenv("MC_TEST_NO_FFMPEG") == ""
	var local []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		local = append(local,
			filepath.Join(dir, name),
			filepath.Join(dir, "bin", name),
			filepath.Join(dir, "ffmpeg", name),
			filepath.Join(dir, "ffmpeg", "bin", name),
		)
		if runtime.GOOS == "darwin" {
			// Inside Media Converter.app/Contents/MacOS, also look in Resources
			// and in the folder that holds the .app.
			local = append(local,
				filepath.Join(dir, "..", "Resources", name),
				filepath.Join(dir, "..", "..", "..", name),
			)
		}
	}
	if systemWide {
		cands = append(cands, local...)
		if p, err := exec.LookPath(name); err == nil {
			cands = append(cands, p)
		}
		cands = append(cands, platformFFmpegPaths()...)
	}

	seen := map[string]bool{}
	lastErr := ""
	for i, c := range cands {
		c = filepath.Clean(c)
		if seen[c] {
			continue
		}
		seen[c] = true
		st, err := os.Stat(c)
		if err != nil || st.IsDir() {
			if i == 0 && manual != "" {
				lastErr = "The FFmpeg you chose can't be found any more."
			}
			continue
		}
		prepareBinary(c)
		info, err := checkFFmpeg(c)
		if err == nil {
			return info
		}
		lastErr = err.Error()
	}
	return FFInfo{Found: false, Error: lastErr}
}

var versionRe = regexp.MustCompile(`(?i)ffmpeg version (\S+)`)

func checkFFmpeg(path string) (FFInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-version")
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return FFInfo{}, fmt.Errorf("%s didn't run: %v", path, err)
	}
	m := versionRe.FindSubmatch(out)
	if m == nil {
		return FFInfo{}, fmt.Errorf("%s isn't FFmpeg", path)
	}
	ver := string(m[1])
	if len(ver) > 0 && ver[0] >= '0' && ver[0] <= '9' {
		if i := strings.IndexAny(ver, "-+~ "); i > 0 {
			ver = ver[:i]
		}
	} else if len(ver) > 14 {
		ver = ver[:14]
	}

	cmd = exec.CommandContext(ctx, path, "-hide_banner", "-encoders")
	hideWindow(cmd)
	out, err = cmd.Output()
	if err != nil {
		return FFInfo{}, fmt.Errorf("%s couldn't list its encoders: %v", path, err)
	}
	enc := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	started := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "------") {
			started = true
			continue
		}
		if !started {
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 {
			enc[f[1]] = true
		}
	}
	return FFInfo{Found: true, Path: path, Version: ver, encoders: enc}, nil
}

func (ff FFInfo) formatViews() []FormatView {
	views := make([]FormatView, 0, len(formats))
	for _, f := range formats {
		v := FormatView{Format: f, Available: true}
		if ff.Found {
			var miss []string
			for _, e := range f.Needs {
				if !ff.encoders[e] {
					miss = append(miss, e)
				}
			}
			if len(miss) > 0 {
				v.Available = false
				v.Missing = strings.Join(miss, ", ")
			}
		}
		views = append(views, v)
	}
	return views
}

// ---------- Reading a file ----------

type ProbeInfo struct {
	Duration     float64
	HasVideo     bool
	HasAudio     bool
	Width        int
	Height       int
	VideoCodec   string
	AudioCodec   string
	MultiChannel bool
}

var (
	durRe  = regexp.MustCompile(`Duration: (\d+):(\d{2}):(\d{2}(?:\.\d+)?)`)
	dimRe  = regexp.MustCompile(`\b(\d{2,5})x(\d{2,5})\b`)
	rotRe  = regexp.MustCompile(`rotation of (-?\d+(?:\.\d+)?) degrees`)
	wordRe = regexp.MustCompile(`^[A-Za-z0-9_]+`)
)

func probe(ffPath, path string) (ProbeInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffPath, "-hide_banner", "-nostdin", "-i", "file:"+path)
	hideWindow(cmd)
	out, _ := cmd.CombinedOutput() // always exits 1: no output file given
	return parseProbe(string(out))
}

func parseProbe(s string) (ProbeInfo, error) {
	var p ProbeInfo
	if m := durRe.FindStringSubmatch(s); m != nil {
		h, _ := strconv.ParseFloat(m[1], 64)
		mi, _ := strconv.ParseFloat(m[2], 64)
		se, _ := strconv.ParseFloat(m[3], 64)
		p.Duration = h*3600 + mi*60 + se
	}
	for _, line := range strings.Split(s, "\n") {
		if !strings.Contains(line, "Stream #") {
			continue
		}
		if i := strings.Index(line, ": Video: "); i >= 0 {
			if strings.Contains(line, "(attached pic)") || p.HasVideo {
				continue
			}
			rest := line[i+len(": Video: "):]
			p.HasVideo = true
			p.VideoCodec = wordRe.FindString(rest)
			if m := dimRe.FindStringSubmatch(rest); m != nil {
				p.Width, _ = strconv.Atoi(m[1])
				p.Height, _ = strconv.Atoi(m[2])
			}
		} else if i := strings.Index(line, ": Audio: "); i >= 0 {
			if p.HasAudio {
				continue
			}
			rest := line[i+len(": Audio: "):]
			p.HasAudio = true
			p.AudioCodec = wordRe.FindString(rest)
			two := strings.Contains(rest, ", mono") || strings.Contains(rest, ", stereo") ||
				strings.Contains(rest, ", 1 channels") || strings.Contains(rest, ", 2 channels")
			p.MultiChannel = !two
		}
	}
	if m := rotRe.FindStringSubmatch(s); m != nil && p.Width > 0 {
		r, _ := strconv.ParseFloat(m[1], 64)
		r = math.Mod(math.Abs(r), 180)
		if math.Abs(r-90) < 1 {
			p.Width, p.Height = p.Height, p.Width
		}
	}
	if !p.HasVideo && !p.HasAudio {
		low := strings.ToLower(s)
		switch {
		case strings.Contains(low, "no such file"):
			return p, errors.New("The file can't be found.")
		case strings.Contains(low, "permission denied") || strings.Contains(low, "operation not permitted"):
			return p, errors.New("Media Converter isn't allowed to open this file.")
		default:
			return p, errors.New("Not a video or audio file FFmpeg can read.")
		}
	}
	return p, nil
}

// ---------- Building the FFmpeg command ----------

func pick(q, high, balanced, small string) string {
	switch q {
	case "high":
		return high
	case "small":
		return small
	default:
		return balanced
	}
}

func fmtSec(s float64) string {
	return strconv.FormatFloat(s, 'f', 3, 64)
}

func scaleFilter(res string, w, h int) string {
	target := map[string]int{"1080": 1080, "720": 720, "480": 480}[res]
	if target == 0 {
		if w%2 == 1 || h%2 == 1 {
			return "scale=trunc(iw/2)*2:trunc(ih/2)*2"
		}
		return ""
	}
	// Limit the short side (works for portrait phone video too) and never upscale.
	return fmt.Sprintf("scale=w='if(gte(iw,ih),-2,trunc(min(%d,iw)/2)*2)':h='if(gte(iw,ih),trunc(min(%d,ih)/2)*2,-2)'", target, target)
}

func buildArgs(j *Job, s Settings, f Format, out string, start float64, hasEnd bool, dur float64) []string {
	a := []string{"-hide_banner", "-nostdin", "-y", "-v", "error", "-progress", "pipe:1", "-nostats"}
	if start > 0 {
		a = append(a, "-ss", fmtSec(start))
	}
	a = append(a, "-i", "file:"+j.Path)
	if hasEnd && dur > 0 {
		a = append(a, "-t", fmtSec(dur))
	}
	q := s.Quality
	opus := func() {
		a = append(a, "-c:a", "libopus", "-b:a", pick(q, "160k", "128k", "96k"))
		if j.MultiChannel {
			a = append(a, "-ac", "2")
		}
	}

	if f.Group == "audio" {
		a = append(a, "-vn", "-sn", "-dn")
		switch f.ID {
		case "mp3":
			a = append(a, "-c:a", "libmp3lame", "-q:a", pick(q, "0", "2", "5"))
		case "m4a":
			a = append(a, "-c:a", "aac", "-b:a", pick(q, "256k", "192k", "128k"), "-movflags", "+faststart")
		case "wav":
			a = append(a, "-c:a", "pcm_s16le")
		case "flac":
			a = append(a, "-c:a", "flac")
		case "ogg":
			opus()
		}
		return append(a, "file:"+out)
	}

	if f.ID == "gif" {
		fps, w := 12, 480
		switch q {
		case "high":
			fps, w = 15, 640
		case "small":
			fps, w = 10, 320
		}
		vf := fmt.Sprintf("fps=%d,scale='min(%d,iw)':-1:flags=lanczos,split[a][b];[a]palettegen=stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle", fps, w)
		a = append(a, "-an", "-sn", "-dn", "-vf", vf, "-loop", "0")
		return append(a, "file:"+out)
	}

	if q == "copy" && f.CanCopy {
		if f.ID == "mkv" {
			a = append(a, "-map", "0:v?")
			if !s.RemoveAudio {
				a = append(a, "-map", "0:a?")
			}
			a = append(a, "-map", "0:s?", "-c", "copy")
		} else {
			a = append(a, "-c", "copy", "-sn", "-dn")
			if s.RemoveAudio {
				a = append(a, "-an")
			}
			if j.VideoCodec == "hevc" {
				a = append(a, "-tag:v", "hvc1")
			}
			a = append(a, "-movflags", "+faststart")
		}
		return append(a, "file:"+out)
	}

	if vf := scaleFilter(s.Resolution, j.Width, j.Height); vf != "" {
		a = append(a, "-vf", vf)
	}
	switch f.ID {
	case "hevc":
		a = append(a, "-c:v", "libx265", "-preset", "medium", "-crf", pick(q, "22", "26", "30"),
			"-pix_fmt", "yuv420p", "-tag:v", "hvc1", "-x265-params", "log-level=error")
	case "webm":
		a = append(a, "-c:v", "libvpx-vp9", "-crf", pick(q, "30", "34", "40"), "-b:v", "0",
			"-row-mt", "1", "-deadline", "good", "-cpu-used", "4", "-pix_fmt", "yuv420p")
	default: // mp4, mov, mkv
		a = append(a, "-c:v", "libx264", "-preset", "medium", "-crf", pick(q, "18", "23", "28"), "-pix_fmt", "yuv420p")
	}
	a = append(a, "-sn", "-dn")
	switch {
	case s.RemoveAudio || !j.HasAudio:
		a = append(a, "-an")
	case f.ID == "webm":
		opus()
	default:
		a = append(a, "-c:a", "aac", "-b:a", pick(q, "192k", "160k", "128k"))
	}
	if f.Ext == "mp4" || f.Ext == "mov" {
		a = append(a, "-movflags", "+faststart")
	}
	a = append(a, "-max_muxing_queue_size", "1024")
	return append(a, "file:"+out)
}

func friendlyError(stderr string, s Settings, f Format) string {
	low := strings.ToLower(stderr)
	has := func(subs ...string) bool {
		for _, x := range subs {
			if strings.Contains(low, x) {
				return true
			}
		}
		return false
	}
	switch {
	case has("no space left"):
		return "The disk is full. Free up some space or pick another folder under Save to."
	case has("permission denied", "operation not permitted"):
		return "Media Converter can't save to that folder. Pick another folder under Save to."
	case s.Quality == "copy" && has("could not find tag for codec", "not currently supported in container", "codec not supported", "only supported in", "incompatible", "tag ", "error initializing output stream", "could not write header"):
		return "The video or sound in this file can't go into " + f.Label + " without converting it. Change Quality from Original to Balanced and try again."
	case has("unknown encoder", "encoder not found"):
		return "Your copy of FFmpeg is missing an encoder that " + f.Label + " needs. Try another format."
	case has("invalid data found", "moov atom not found", "end of file"):
		return "This file looks damaged or incomplete, so FFmpeg couldn't read all of it."
	default:
		return "FFmpeg couldn't convert this file. Open Details to see what it reported."
	}
}

// ---------- Times ----------

var errBadTime = errors.New("bad time")

// parseTime accepts 90, 1:30, 01:02:03 and decimals like 1:30.5.
func parseTime(s string) (float64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if s == "" {
		return 0, nil
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, errBadTime
	}
	total := 0.0
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return 0, errBadTime
		}
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, errBadTime
		}
		if i > 0 && v >= 60 {
			return 0, errBadTime
		}
		total = total*60 + v
	}
	return total, nil
}
