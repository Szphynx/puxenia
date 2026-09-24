// push-sample-fetch: a tiny standalone web service meant to run directly on
// Push 3's own embedded Linux (not on a laptop). It exists to close one
// specific gap: Ableton Live's own browser/Simpler UI has no "fetch this
// from the internet" button. This gives you one, reachable from a phone on
// the same wifi as Push — paste a video URL (Instagram, YouTube, etc.), it
// downloads, extracts + cleans up the audio with yt-dlp/ffmpeg, and drops a
// ready-to-use .wav directly into a folder Live's own User Library browser
// watches. Loading it into Simpler afterward is then just Push's own
// native browse-and-load, no extra automation needed or attempted here —
// see README.md for why "auto-load into a specific device slot" isn't part
// of this tool, and for the SAMPLE_DIR path that must be confirmed on real
// hardware before this is useful at all.
//
// Unlike push-hack-xenia/push-hack-mm, this has no MIDI, no ALSA audio
// render loop, and no on-screen UI — it never touches Push's pads, screen,
// or push-manager/push-display at all, so none of push-hub's focus-
// arbitration contract applies to it. It's a plain HTTP server, deployed
// and run the same way (scp + nohup over SSH) as everything else in this
// repo.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type config struct {
	port          int
	sampleDir     string
	workDir       string
	ytdlpPath     string
	ffmpegPath    string
	cookiesFile   string
	maxFilesizeMB int
	fetchTimeout  time.Duration
}

func main() {
	cfg := config{}
	flag.IntVar(&cfg.port, "port", 7710, "TCP port to listen on (reachable from any device on Push's wifi, same as the other hacks' web UIs)")
	flag.StringVar(&cfg.sampleDir, "sample-dir", os.Getenv("SAMPLE_DIR"), "REQUIRED: directory Live's own User Library browser watches on this Push. Not guessed — confirm it on real hardware first, see README.md")
	flag.StringVar(&cfg.workDir, "work-dir", "./work", "scratch directory for in-progress downloads (cleaned up per-request)")
	flag.StringVar(&cfg.ytdlpPath, "ytdlp", "./yt-dlp", "path to the yt-dlp binary")
	flag.StringVar(&cfg.ffmpegPath, "ffmpeg", "./ffmpeg", "path to the ffmpeg binary")
	flag.StringVar(&cfg.cookiesFile, "cookies", os.Getenv("YTDLP_COOKIES"), "optional cookies.txt (Netscape format) for private/login-gated source posts")
	flag.IntVar(&cfg.maxFilesizeMB, "max-filesize-mb", 200, "reject source downloads bigger than this — a basic guard against accidentally grabbing a long video as a 'sample'")
	flag.DurationVar(&cfg.fetchTimeout, "fetch-timeout", 5*time.Minute, "hard timeout for the download+convert pipeline per request")
	flag.Parse()

	if cfg.sampleDir == "" {
		log.Fatal("FATAL: -sample-dir (or SAMPLE_DIR) is required and has no default — " +
			"this is Live's User Library sample folder on THIS Push, which this tool has " +
			"no way to know on its own. See README.md's \"finding your Push's sample folder\" section.")
	}
	if err := os.MkdirAll(cfg.sampleDir, 0o755); err != nil {
		log.Fatalf("FATAL: sample-dir %q not usable: %v", cfg.sampleDir, err)
	}
	if err := os.MkdirAll(cfg.workDir, 0o755); err != nil {
		log.Fatalf("FATAL: work-dir %q not usable: %v", cfg.workDir, err)
	}
	for _, bin := range []struct{ name, path string }{{"yt-dlp", cfg.ytdlpPath}, {"ffmpeg", cfg.ffmpegPath}} {
		if _, err := os.Stat(bin.path); err != nil {
			log.Fatalf("FATAL: %s not found at %q (%v) — deploy.sh should have placed it alongside this binary", bin.name, bin.path, err)
		}
	}

	srv := &server{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleIndex)
	mux.HandleFunc("POST /fetch", srv.handleFetch)
	mux.HandleFunc("GET /api/samples", srv.handleAPISamples)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })

	addr := fmt.Sprintf(":%d", cfg.port)
	log.Printf("push-sample-fetch listening on %s — sample-dir=%s", addr, cfg.sampleDir)
	log.Printf("open this from any browser on the same wifi as Push, e.g. http://<push-ip>:%d/", cfg.port)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

type server struct {
	cfg      config
	fetchSem chan struct{} // size-1: one download+convert at a time, Push's CPU is modest and shared with other hacks
	once     sync.Once
}

func (s *server) sem() chan struct{} {
	s.once.Do(func() { s.fetchSem = make(chan struct{}, 1) })
	return s.fetchSem
}

// --- pages ---

var pageTmpl = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>push-sample-fetch</title>
<style>
body{font-family:-apple-system,system-ui,sans-serif;max-width:640px;margin:2rem auto;padding:0 1rem;background:#111;color:#eee}
h1{font-size:1.3rem} label{display:block;margin-top:1rem;font-size:.9rem;color:#aaa}
input[type=text],input[type=url]{width:100%;padding:.6rem;font-size:1rem;box-sizing:border-box;background:#222;color:#eee;border:1px solid #444;border-radius:6px}
button{margin-top:1.2rem;padding:.7rem 1.4rem;font-size:1rem;background:#4a9;color:#fff;border:none;border-radius:6px}
.row{display:flex;gap:.6rem} .row>div{flex:1}
.msg{margin-top:1rem;padding:.8rem;border-radius:6px;white-space:pre-wrap;font-family:monospace;font-size:.85rem}
.ok{background:#183;color:#dfd} .err{background:#611;color:#fdd}
ul{padding-left:1.2rem} li{margin:.2rem 0;font-size:.85rem;color:#ccc}
a{color:#7cf}
</style></head><body>
<h1>push-sample-fetch</h1>
<p style="color:#999;font-size:.85rem">Paste a video URL (Instagram, YouTube, etc.) — the audio gets downloaded,
cleaned up, and dropped into Live's own sample folder on this Push. Load it from there in Simpler with Push's
own browser, same as any other sample.</p>
<form method="post" action="/fetch">
<label>Video URL</label>
<input type="url" name="url" placeholder="https://www.instagram.com/reel/..." required>
<div class="row">
<div><label>Start (sec, optional)</label><input type="text" name="start" placeholder="e.g. 12"></div>
<div><label>Duration (sec, optional)</label><input type="text" name="duration" placeholder="e.g. 4"></div>
</div>
<label><input type="checkbox" name="normalize" checked style="width:auto"> Normalize loudness</label>
<button type="submit">Fetch sample</button>
</form>
{{if .Message}}<div class="msg {{if .OK}}ok{{else}}err{{end}}">{{.Message}}</div>{{end}}
<h2 style="font-size:1rem;margin-top:2rem;color:#999">Recent samples in {{.SampleDir}}</h2>
<ul>{{range .Recent}}<li>{{.}}</li>{{else}}<li>(none yet)</li>{{end}}</ul>
</body></html>`))

type pageData struct {
	Message   string
	OK        bool
	SampleDir string
	Recent    []string
}

func (s *server) recentSamples(n int) []string {
	entries, err := os.ReadDir(s.cfg.sampleDir)
	if err != nil {
		return nil
	}
	type fi struct {
		name string
		mod  time.Time
	}
	var files []fi
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fi{e.Name(), info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	if len(files) > n {
		files = files[:n]
	}
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.name
	}
	return out
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, pageData{SampleDir: s.cfg.sampleDir, Recent: s.recentSamples(15)})
}

func (s *server) renderPage(w http.ResponseWriter, data pageData) {
	if data.SampleDir == "" {
		data.SampleDir = s.cfg.sampleDir
	}
	if data.Recent == nil {
		data.Recent = s.recentSamples(15)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.Execute(w, data); err != nil {
		log.Printf("template error: %v", err)
	}
}

// --- fetch pipeline ---

func (s *server) handleFetch(w http.ResponseWriter, r *http.Request) {
	select {
	case s.sem() <- struct{}{}:
		defer func() { <-s.sem() }()
	default:
		s.renderPage(w, pageData{OK: false, Message: "Already fetching another sample — wait for it to finish and try again."})
		return
	}

	if err := r.ParseForm(); err != nil {
		s.renderPage(w, pageData{OK: false, Message: "Bad form submission: " + err.Error()})
		return
	}
	rawURL := strings.TrimSpace(r.FormValue("url"))
	start := strings.TrimSpace(r.FormValue("start"))
	duration := strings.TrimSpace(r.FormValue("duration"))
	normalize := r.FormValue("normalize") != ""

	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		s.renderPage(w, pageData{OK: false, Message: "Not a valid http(s) URL: " + rawURL})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.fetchTimeout)
	defer cancel()

	destPath, sizeBytes, err := s.fetchAndConvert(ctx, rawURL, start, duration, normalize)
	if err != nil {
		s.renderPage(w, pageData{OK: false, Message: "Failed: " + err.Error()})
		return
	}
	s.renderPage(w, pageData{
		OK: true,
		Message: fmt.Sprintf("Saved %s (%.1f KB) to %s\n\nIf it doesn't show up in Live's browser yet, "+
			"hit Rescan on the User Library folder once (on Push's own screen) — see README.md.",
			filepath.Base(destPath), float64(sizeBytes)/1024, s.cfg.sampleDir),
	})
}

var titleSanitizer = regexp.MustCompile(`[^a-zA-Z0-9\-]+`)

func slugify(title string) string {
	title = strings.ToLower(strings.TrimSpace(title))
	title = titleSanitizer.ReplaceAllString(title, "-")
	title = strings.Trim(title, "-")
	if len(title) > 40 {
		title = title[:40]
	}
	if title == "" {
		title = "sample"
	}
	return title
}

// fetchAndConvert downloads rawURL's audio with yt-dlp, cleans it up with
// ffmpeg (optional trim/normalize, always a short click-safe fade in/out —
// see the fade filter chain below), and moves the result into sampleDir.
// Returns the destination path and its size.
func (s *server) fetchAndConvert(ctx context.Context, rawURL, start, duration string, normalize bool) (string, int64, error) {
	tmpDir, err := os.MkdirTemp(s.cfg.workDir, "fetch-*")
	if err != nil {
		return "", 0, fmt.Errorf("creating work dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Title lookup is best-effort and separate from the download itself —
	// a failure here must not fail the whole fetch, it just falls back to
	// a plain "sample-<timestamp>" name.
	title := ""
	{
		titleCtx, titleCancel := context.WithTimeout(ctx, 20*time.Second)
		out, _ := exec.CommandContext(titleCtx, s.cfg.ytdlpPath, "--skip-download", "--print", "%(title).60s", rawURL).Output()
		titleCancel()
		title = strings.TrimSpace(string(out))
	}

	srcPath := filepath.Join(tmpDir, "src.wav")
	ytArgs := []string{
		"-x", "--audio-format", "wav", "--audio-quality", "0",
		"--no-playlist",
		"--max-filesize", fmt.Sprintf("%dM", s.cfg.maxFilesizeMB),
		"-o", filepath.Join(tmpDir, "src.%(ext)s"),
	}
	if s.cfg.cookiesFile != "" {
		ytArgs = append(ytArgs, "--cookies", s.cfg.cookiesFile)
	}
	ytArgs = append(ytArgs, rawURL)

	if out, err := runCapture(ctx, s.cfg.ytdlpPath, ytArgs...); err != nil {
		return "", 0, fmt.Errorf("yt-dlp failed: %v\n%s", err, tail(out, 20))
	}
	if _, err := os.Stat(srcPath); err != nil {
		return "", 0, fmt.Errorf("yt-dlp reported success but %s is missing — no audio track in that URL?", filepath.Base(srcPath))
	}

	// Always apply a short click-safe fade in/out (the areverse trick fades
	// the tail without needing to know the clip's duration up front), and
	// optionally EBU R128 loudness normalization before the fades so the
	// fade envelope itself isn't renormalized afterward.
	filters := []string{}
	if normalize {
		filters = append(filters, "loudnorm=I=-16:TP=-1.5:LRA=11")
	}
	filters = append(filters, "afade=t=in:d=0.005", "areverse", "afade=t=in:d=0.005", "areverse")

	outPath := filepath.Join(tmpDir, "out.wav")
	ffArgs := []string{"-y", "-i", srcPath}
	if start != "" {
		if _, err := strconv.ParseFloat(start, 64); err != nil {
			return "", 0, fmt.Errorf("start must be a number of seconds, got %q", start)
		}
		ffArgs = append(ffArgs, "-ss", start)
	}
	if duration != "" {
		if _, err := strconv.ParseFloat(duration, 64); err != nil {
			return "", 0, fmt.Errorf("duration must be a number of seconds, got %q", duration)
		}
		ffArgs = append(ffArgs, "-t", duration)
	}
	ffArgs = append(ffArgs,
		"-af", strings.Join(filters, ","),
		"-ar", "44100", "-ac", "2", "-sample_fmt", "s16",
		outPath,
	)
	if out, err := runCapture(ctx, s.cfg.ffmpegPath, ffArgs...); err != nil {
		return "", 0, fmt.Errorf("ffmpeg failed: %v\n%s", err, tail(out, 20))
	}

	info, err := os.Stat(outPath)
	if err != nil {
		return "", 0, fmt.Errorf("ffmpeg reported success but output is missing: %w", err)
	}

	finalName := fmt.Sprintf("%s-%d.wav", slugify(title), time.Now().Unix())
	destPath := filepath.Join(s.cfg.sampleDir, finalName)
	if err := moveFile(outPath, destPath); err != nil {
		return "", 0, fmt.Errorf("moving into sample dir: %w", err)
	}
	return destPath, info.Size(), nil
}

func runCapture(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// os.Rename fails with EXDEV when src/dst are on different filesystems
	// (workDir vs sampleDir need not be the same mount) — fall back to a
	// copy in that case, and in any other rename failure too.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// --- JSON API ---

type sampleInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

func (s *server) handleAPISamples(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.cfg.sampleDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var out []sampleInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, sampleInfo{e.Name(), info.Size(), info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
