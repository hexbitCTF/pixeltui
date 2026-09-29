// Package player is pixeltui's headless audio engine: it resolves a track to a
// playable stream and drives mpv (with ffplay/afplay fallbacks) over IPC. It has
// no dependency on the terminal UI, so any front-end — the TUI, the pocket
// hardware client, or the server — can reuse it. The TUI wraps these calls in
// tea.Cmds; pocket consumes Watch()/Media() directly.
package player

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dotjarden/pixeltui/tui/engine"
	"github.com/dotjarden/pixeltui/tui/innertube"
	"github.com/dotjarden/pixeltui/tui/output"
	"github.com/dotjarden/pixeltui/tui/ytm"
)

// ── playback ──────────────────────────────────────────────────────────────────

// Stream holds one active audio stream.
type Stream struct {
	cmd       *exec.Cmd
	dl        *exec.Cmd // yt-dlp feeder (pipe mode only)
	socket    string    // mpv IPC socket path (empty → no IPC control)
	ended     <-chan struct{}
	media     <-chan MediaCmd // OS/hardware transport commands (mpv only)
	mediaStop func()          // tears down the media reader

	stopOnce sync.Once // Stop closes mediaStop's channel, so it must run once
}

// Attach wraps an already-running mpv IPC socket as a controllable Stream
// without spawning a process — for front-ends that drive a player they didn't
// start (and for tests). ended is closed when the underlying player exits; an
// open channel means "still playing".
func Attach(socket string, ended <-chan struct{}) *Stream {
	return &Stream{socket: socket, ended: ended}
}

// Ended reports whether the underlying player process has exited.
func (s *Stream) Ended() bool {
	if s == nil || s.ended == nil {
		return true
	}
	select {
	case <-s.ended:
		return true
	default:
		return false
	}
}

// CanControl reports whether IPC control (pause/seek/volume) is available.
func (s *Stream) CanControl() bool {
	return s != nil && s.socket != "" && !s.Ended()
}

// Stop kills the player (and any yt-dlp feeder) and cleans up the IPC socket.
//
// It is safe to call more than once. mediaStop closes a channel, so a second
// call used to panic with "close of closed channel" -- and it did in practice:
// Run() returns once the player ends, and the deferred fm.now.Stop() in
// cmdRecommend then stopped an already-stopped stream. A plain repeated call
// (a UI that tears down on both quit and error) hit it too.
func (s *Stream) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.mediaStop != nil {
			s.mediaStop()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			s.cmd.Process.Kill() //nolint:errcheck
		}
		if s.ended != nil {
			<-s.ended
		}
		if s.dl != nil && s.dl.Process != nil {
			s.dl.Process.Kill() //nolint:errcheck
			s.dl.Wait()         //nolint:errcheck
		}
		if s.socket != "" {
			removeIPC(s.socket)
		}
	})
}

// Media returns the channel of OS/hardware transport commands (nil for a
// non-mpv fallback player). On pocket hardware the GPIO buttons feed this same
// channel; re-read it after each command to keep listening.
func (s *Stream) Media() <-chan MediaCmd {
	if s == nil {
		return nil
	}
	return s.media
}

// CurrentEntryID reports the playlist entry id mpv is currently on (0 if
// unknown) — how a poll loop notices a gapless auto-advance.
func (s *Stream) CurrentEntryID() int {
	if !s.CanControl() {
		return 0
	}
	for _, e := range ipcPlaylist(s.socket) {
		if e.Current {
			return e.ID
		}
	}
	return 0
}

func (s *Stream) Pause() {
	if s.CanControl() {
		ipcCmd(s.socket, "cycle", "pause")
	}
}
func (s *Stream) Seek(sec float64) {
	if s.CanControl() {
		ipcCmd(s.socket, "seek", sec, "relative")
	}
}

// Restart seeks the current track back to the beginning (OS "previous" → restart).
func (s *Stream) Restart() {
	if s.CanControl() {
		ipcCmd(s.socket, "seek", 0, "absolute")
	}
}
func (s *Stream) Volume() int {
	if !s.CanControl() {
		return -1
	}
	return int(ipcFloat(s.socket, "volume"))
}
func (s *Stream) SetVolume(v int) {
	if s.CanControl() {
		ipcCmd(s.socket, "set_property", "volume", float64(v))
	}
}
func (s *Stream) IsPaused() bool { return s.CanControl() && ipcBool(s.socket, "pause") }
func (s *Stream) Position() float64 {
	if !s.CanControl() {
		return 0
	}
	return ipcFloat(s.socket, "time-pos")
}
func (s *Stream) Duration() float64 {
	if !s.CanControl() {
		return 0
	}
	return ipcFloat(s.socket, "duration")
}

// SetTitle updates the OS Now Playing title of the current entry over IPC.
func (s *Stream) SetTitle(title string) {
	if s.CanControl() {
		ipcCmd(s.socket, "set_property", "force-media-title", title)
	}
}

// Gapless reconciles mpv's playlist with the queue head: it drops a stale
// previously-appended entry (removeID; 0 = none) and, if url != "", appends the
// next track right after the current one so the natural end-of-track boundary
// plays on inside the running mpv (no respawn). Returns the new entry id.
func (s *Stream) Gapless(removeID int, url, title, cover string) (int, error) {
	if removeID != 0 {
		if idx := playlistIndexOfID(s.socket, removeID); idx >= 0 {
			ipcCmd(s.socket, "playlist-remove", idx)
		}
	}
	if url == "" {
		return 0, nil
	}
	return gaplessAppend(s.socket, url, title, cover)
}

// State is a snapshot of playback for headless consumers (pocket, serve) that
// can't use the TUI's tea-driven poll loop.
type State struct {
	Pos     float64
	Dur     float64
	Paused  bool
	Vol     int
	EntryID int
	Ended   bool
}

// Watch polls the stream every `every` and emits a State snapshot on the
// returned channel until the stream ends or ctx is cancelled; the channel is
// closed on exit. Intended for headless front-ends — the TUI keeps its own
// tea-driven poll.
func (s *Stream) Watch(ctx context.Context, every time.Duration) <-chan State {
	out := make(chan State, 1)
	go func() {
		defer close(out)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if s == nil || s.Ended() {
					select {
					case out <- State{Ended: true}:
					case <-ctx.Done():
					}
					return
				}
				select {
				case out <- State{
					Pos:     s.Position(),
					Dur:     s.Duration(),
					Paused:  s.IsPaused(),
					Vol:     s.Volume(),
					EntryID: s.CurrentEntryID(),
				}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// ── starting mpv ──────────────────────────────────────────────────────────────

// audioDevice is the legacy --audio-device fallback, used when no output
// registry has been installed.
var audioDevice string

// outputReg is the active sink registry; nil until SetAudioDevice or
// SetOutputRegistry installs it.
var outputReg *output.Registry

// SetAudioDevice pins the mpv --audio-device for subsequently started streams.
// Empty (the default) uses mpv's default output device. It also updates the
// "mpv-device" sink in the output registry, so capability-driven menus see it.
func SetAudioDevice(dev string) {
	audioDevice = dev
	if outputReg == nil {
		outputReg = output.NewRegistry()
		// mpv-device comes first so it wins when a device is configured; default
		// catches the empty/unconfigured case.
		outputReg.Register(&output.MPVDevice{Device: dev})
		outputReg.Register(output.Default())
		return
	}
	if s, ok := outputReg.ByKey("mpv-device").(*output.MPVDevice); ok {
		s.Device = dev
		return
	}
	outputReg.Register(&output.MPVDevice{Device: dev})
}

// SetOutputRegistry replaces the active sink registry. Callers that want a
// custom sink list (e.g. a Bluetooth or AirPlay renderer) use this; the
// registry is still consulted in mpvBaseArgs.
func SetOutputRegistry(r *output.Registry) {
	outputReg = r
}

// OutputRegistry returns the active sink registry, creating a default one if
// needed. Useful for menus that list available sinks.
func OutputRegistry() *output.Registry {
	if outputReg == nil {
		outputReg = output.NewRegistry()
		outputReg.Register(output.Default())
	}
	return outputReg
}

func watchEnded(cmd *exec.Cmd) <-chan struct{} {
	ch := make(chan struct{})
	go func() { cmd.Wait(); close(ch) }() //nolint:errcheck
	return ch
}

// awaitSocket waits up to 4s for mpv's IPC endpoint to become connectable.
func awaitSocket(path string) bool {
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if ipcReady(path) {
			return true
		}
		time.Sleep(40 * time.Millisecond)
	}
	return false
}

func mpvBaseArgs(socket, title, coverPath string) []string {
	args := []string{
		// --vo=null (not --no-video): decodes the cover-art image so it reaches
		// the OS Now Playing widget, but opens NO window (verified windowless).
		"--vo=null",
		"--ytdl-format=bestaudio/best",
		// Pin the fast extractor client for mpv's internal yt-dlp (fallback path
		// only). MUST use -append: plain --ytdl-raw-options comma-splits the
		// value, so "visionos,web" would break mpv startup entirely.
		"--ytdl-raw-options-append=extractor-args=youtube:player_client=visionos,web",
		"--no-terminal",
		"--really-quiet",
		"--input-ipc-server=" + socket,
	}
	// Apply the active output sink (pocket DAC, Bluetooth, AirPlay, etc.).
	// Falls back to the legacy audioDevice variable if no registry is set.
	if outputReg != nil {
		args, _ = outputReg.Apply("", args)
	} else if audioDevice != "" {
		args = append(args, "--audio-device="+audioDevice)
	}
	if title != "" {
		args = append(args, "--force-media-title="+title)
	}
	// Pixelated terminal-style cover art for the OS Now Playing widget (lol).
	if coverPath != "" {
		args = append(args, "--cover-art-files="+coverPath)
	}
	// OS "Now Playing" integration: macOS Control Center / MPNowPlayingInfoCenter,
	// Windows SMTC, Linux MPRIS — all via mpv's --media-controls.
	args = append(args, "--media-controls=yes")
	if runtime.GOOS == "darwin" {
		// Media keys + keep mpv out of the Dock while it owns Now Playing.
		args = append(args,
			"--input-media-keys=yes",
			"--macos-app-activation-policy=accessory",
		)
	}
	return args
}

// launchMPV starts mpv on source (direct CDN URL or a youtube watch URL) and
// waits for the IPC socket so pause/seek/volume work immediately. coverPath, if
// set, is the pixelated cover shown in the OS Now Playing widget.
func launchMPV(mpvPath, source, track, artist, coverPath string) (*Stream, error) {
	sock := mpvSocketPath()
	args := append(mpvBaseArgs(sock, track+" — "+artist, coverPath), source)

	cmd := exec.Command(mpvPath, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		removeIPC(sock)
		return nil, err
	}
	if !awaitSocket(sock) {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		removeIPC(sock)
		return nil, fmt.Errorf("mpv: IPC socket not ready")
	}
	s := &Stream{cmd: cmd, socket: sock, ended: watchEnded(cmd)}
	// Bridge OS / hardware media controls (next/prev/play-pause) to the app queue.
	s.media, s.mediaStop = startMediaReader(sock)
	padSkipPlaylist(sock)
	return s, nil
}

// padSkipPlaylist gives mpv tiny silent neighbour entries so on-screen transport
// controls that drive mpv's playlist directly (Windows SMTC, and others that
// bypass the input/keybind layer) have a Next/Prev to move to — otherwise mpv
// disables those buttons on a 1-item playlist.
//
// Playlist becomes [silence, current, silence] with the current track at index
// 1. Next → the trailing silence plays out (≈50ms) → mpv exits → the existing
// auto-advance plays the queue's next track. Prev → the leading silence plays
// out → mpv returns to the current track from the start (= restart).
//
// Harmless if unsupported: if the silent source or insert-at isn't available the
// entries simply don't enable the buttons (no worse than before).
func padSkipPlaylist(socket string) {
	const sentinel = "av://lavfi:anullsrc=d=0.05"        // ~50ms of silence, finite
	ipcCmd(socket, "loadfile", sentinel, "append")       // next slot  → index 1→2
	ipcCmd(socket, "loadfile", sentinel, "insert-at", 0) // prev slot  → current →1
}

// ytExtractorArgs pins YouTube player clients for extraction speed.
//
// The default behaviour probes several clients serially (~24s here). The
// "visionos" client returns a clean, audio-only, pre-signed URL with no
// "n" signature to compute and no PO-token requirement — typically ~2× faster
// and ffplay/mpv-compatible (opus/webm). "web" is kept as a resilient fallback
// in case visionos is ever blocked.
var ytExtractorArgs = []string{"--extractor-args", "youtube:player_client=visionos,web"}

// withYT prepends the shared fast-extraction flags to a yt-dlp arg list.
func withYT(args ...string) []string {
	return append(append([]string{}, ytExtractorArgs...), args...)
}

// findMPV resolves mpv: $PIXELTUI_MPV → data-dir install → PATH. The data-dir
// path lets `doctor --fix` install mpv into ~/.pixeltui and have the app find it
// without touching the system PATH (macOS bundle, or the Windows standalone
// build under ~/.pixeltui/mpv).
func findMPV() string {
	if p := os.Getenv("PIXELTUI_MPV"); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, cand := range []string{
			filepath.Join(home, ".pixeltui", "mpv.app", "Contents", "MacOS", "mpv"), // macOS bundle
			filepath.Join(home, ".pixeltui", "mpv", "mpv.exe"),                      // Windows build
		} {
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand
			}
		}
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" { // winget portable shim (Windows)
		cand := filepath.Join(la, "Microsoft", "WinGet", "Links", "mpv.exe")
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand
		}
	}
	if p, err := exec.LookPath("mpv"); err == nil {
		return p
	}
	return ""
}

// MPVAvailable reports whether mpv is installed (gates playback controls).
func MPVAvailable() bool { return findMPV() != "" }

// YtdlpPath returns the preferred yt-dlp, in priority order:
//  1. $PIXELTUI_YTDLP (explicit override)
//  2. ~/.pixeltui/ytdlp-venv/bin/yt-dlp  (pip install)
//  3. ~/.pixeltui/bin/yt-dlp             (standalone, doctor --fix)
//  4. yt-dlp on PATH
func YtdlpPath() string {
	if p := os.Getenv("PIXELTUI_YTDLP"); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		venv := filepath.Join(home, ".pixeltui", "ytdlp-venv")
		for _, cand := range []string{
			filepath.Join(venv, "bin", "yt-dlp"),         // macOS/Linux venv
			filepath.Join(venv, "Scripts", "yt-dlp.exe"), // Windows venv
		} {
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand
			}
		}
		bin := filepath.Join(home, ".pixeltui", "bin", "yt-dlp")
		if runtime.GOOS == "windows" {
			bin = filepath.Join(home, ".pixeltui", "bin", "yt-dlp.exe")
		}
		if fi, err := os.Stat(bin); err == nil && !fi.IsDir() {
			return bin
		}
	}
	if p, err := exec.LookPath("yt-dlp"); err == nil {
		return p
	}
	return ""
}

// Cache caches resolved CDN URLs to disk (implemented by store.Cache).
type Cache interface {
	GetStreamURL(videoID string) (string, bool)
	PutStreamURL(videoID, url string, expire int64)
}

// streamCache is set by SetCache; nil disables caching.
var streamCache Cache

// SetCache installs the resolved-URL disk cache (nil disables caching).
func SetCache(c Cache) { streamCache = c }

// offline gates network stream resolution: when set, resolveStreamURL skips
// InnerTube/yt-dlp and fails fast instead of hanging. Cached URLs and local
// files (StreamURL set) still play. A connectivity.Monitor drives this.
var offline atomic.Bool

// SetOffline toggles the connectivity gate on the resolver.
func SetOffline(b bool) { offline.Store(b) }

// Offline reports whether network resolution is currently gated off.
func Offline() bool { return offline.Load() }

// Resolve turns a video id into a direct CDN audio URL (InnerTube first, yt-dlp
// fallback), caching the result by video id. Convenience wrapper that resolves
// the yt-dlp path itself.
func Resolve(videoID string) (string, error) {
	return resolveStreamURL(YtdlpPath(), videoID)
}

// resolveStreamURL turns a video id into a direct CDN audio URL. The native
// InnerTube resolver runs first (~0.2s, single HTTP call, no subprocess);
// yt-dlp is only the fallback for rare playability quirks, so playback works
// without yt-dlp installed at all. Results are cached to disk by video id
// until the CDN URL's `expire` time, so replays/restarts are instant.
func resolveStreamURL(ytdlp, videoID string) (string, error) {
	if videoID == "" {
		return "", fmt.Errorf("no video id")
	}
	if streamCache != nil {
		if u, ok := streamCache.GetStreamURL(videoID); ok {
			return u, nil
		}
	}
	if offline.Load() { // no connectivity → don't hang on network resolution
		return "", fmt.Errorf("offline: no cached stream for %s", videoID)
	}

	if res, err := innertube.Resolve(context.Background(), videoID); err == nil && res.URL != "" {
		if streamCache != nil {
			streamCache.PutStreamURL(videoID, res.URL, res.Expire)
		}
		return res.URL, nil
	}

	if ytdlp == "" {
		return "", fmt.Errorf("stream resolution failed (no yt-dlp fallback installed)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, ytdlp,
		withYT("--get-url", "-f", "bestaudio/best", "--no-playlist", "--quiet", ytm.WatchURL(videoID))...,
	).Output()
	if err != nil {
		return "", err
	}
	u := strings.SplitN(strings.TrimSpace(string(raw)), "\n", 2)[0]
	if u == "" {
		return "", fmt.Errorf("no stream URL")
	}
	if streamCache != nil {
		streamCache.PutStreamURL(videoID, u, expireOf(u))
	}
	return u, nil
}

// expireOf reads the googlevideo `expire=` unix timestamp; falls back to +5h.
func expireOf(cdnURL string) int64 {
	i := strings.Index(cdnURL, "expire=")
	if i >= 0 {
		s := cdnURL[i+7:]
		if j := strings.IndexByte(s, '&'); j >= 0 {
			s = s[:j]
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
	}
	return time.Now().Add(5 * time.Hour).Unix()
}

// EnsureVideoID enriches a candidate with a YouTube Music video id (+ duration
// and art) if it doesn't already have one. Recommender candidates arrive bare;
// ytmusic search results already carry these.
func EnsureVideoID(c engine.Candidate) engine.Candidate {
	if c.VideoID != "" {
		return c
	}
	if r, err := ytm.Resolve(c.Artist, c.Track); err == nil {
		c.VideoID = r.VideoID
		c.DurationSec = r.DurationSec
		c.ArtURL = r.ArtURL
	}
	return c
}

// Start begins streaming a candidate. We ALWAYS resolve the direct CDN URL
// ourselves (fast visionos client) and hand it to the player — mpv's internal
// ytdl hook is slow/fragile (it hangs on music.youtube URLs), so we never rely
// on it. Resolution order:
//  1. resolve CDN URL (preloaded if available, else InnerTube/yt-dlp)
//  2. play it: mpv (IPC controls) → ffplay (opus-capable)
//  3. fallback: yt-dlp | ffplay pipe, then afplay proxy (m4a) — for odd cases
//
// Returns the (possibly enriched) candidate so the UI gets duration/art.
func Start(c engine.Candidate, preloadedURL string) (*Stream, engine.Candidate, error) {
	mpvPath := findMPV()

	// Direct-URL sources (e.g. Subsonic): the track already has a playable URL,
	// so skip ytmusic resolution and yt-dlp entirely.
	if c.StreamURL != "" {
		cover := ""
		if mpvPath != "" {
			cover = CoverFor(c.ArtURL)
		}
		if s, err := playDirectURL(mpvPath, c.StreamURL, cover, c); err == nil {
			return s, c, nil
		}
		// fall through to generic handling if the direct play somehow failed
	}

	c = EnsureVideoID(c)
	ytdlp := YtdlpPath()

	// Pixelated cover for the OS Now Playing widget (mpv only; cached/preloaded).
	cover := ""
	if mpvPath != "" {
		cover = CoverFor(c.ArtURL)
	}

	var watchURL string
	if c.VideoID != "" {
		watchURL = ytm.WatchURL(c.VideoID)
	}

	// ── 1. Resolve a direct CDN URL ourselves (never mpv's internal hook) ──────
	cdnURL := preloadedURL
	if cdnURL == "" && c.VideoID != "" {
		if u, err := resolveStreamURL(ytdlp, c.VideoID); err == nil {
			cdnURL = u
		}
	}

	// ── 2. Play the direct URL: mpv (controls) preferred, else ffplay ─────────
	if cdnURL != "" {
		if s, err := playDirectURL(mpvPath, cdnURL, cover, c); err == nil {
			return s, c, nil
		}
	}

	// ── 3. Fallbacks (resolution failed, or no direct-URL player available) ───
	target := watchURL
	if target == "" {
		target = "ytsearch1:" + c.Track + " " + c.Artist
	}

	// 3a. mpv on the search/watch target (last resort: uses its ytdl hook).
	if mpvPath != "" && cdnURL == "" {
		if s, err := launchMPV(mpvPath, target, c.Track, c.Artist, cover); err == nil {
			return s, c, nil
		}
	}

	if ytdlp == "" {
		return nil, c, fmt.Errorf("stream resolution failed and yt-dlp (fallback) not found\n%s", ytdlpInstall())
	}

	// 3b. yt-dlp | ffplay pipe.
	if ffplay, _ := exec.LookPath("ffplay"); ffplay != "" && playerValid(ffplay, "-version") {
		dl := exec.Command(ytdlp, withYT("-f", "bestaudio/best", "-o", "-", "--quiet", "--no-playlist", target)...)
		pl := exec.Command(ffplay, "-nodisp", "-autoexit", "-loglevel", "quiet", "-i", "pipe:0")
		if s, err := pipePlay(dl, pl); err == nil {
			return s, c, nil
		}
	}

	// 3c. afplay proxy (re-resolves an m4a stream itself).
	if afplay, _ := exec.LookPath("afplay"); afplay != "" {
		s, err := afplayProxy(ytdlp, afplay, target)
		return s, c, err
	}

	return nil, c, fmt.Errorf("no player found\n%s", playerInstall())
}

// playDirectURL plays an already-resolved CDN URL with no yt-dlp at play time.
// Prefers mpv (IPC control); else ffplay (handles opus/webm). Returns an error
// if neither is available so the caller can fall back to the resolve path.
func playDirectURL(mpvPath, url, coverPath string, c engine.Candidate) (*Stream, error) {
	if mpvPath != "" {
		return launchMPV(mpvPath, url, c.Track, c.Artist, coverPath)
	}
	ffplay, _ := exec.LookPath("ffplay")
	if ffplay == "" || !playerValid(ffplay, "-version") {
		return nil, fmt.Errorf("no direct-url player")
	}
	pl := exec.Command(ffplay, "-nodisp", "-autoexit", "-loglevel", "quiet", "-i", url)
	pl.Stderr = io.Discard
	if err := pl.Start(); err != nil {
		return nil, err
	}
	ended := make(chan struct{})
	go func() { pl.Wait(); close(ended) }() //nolint:errcheck
	select {
	case <-ended:
		return nil, fmt.Errorf("ffplay crashed on startup")
	case <-time.After(350 * time.Millisecond):
	}
	return &Stream{cmd: pl, ended: ended}, nil
}

func pipePlay(dl, pl *exec.Cmd) (*Stream, error) {
	pipe, err := dl.StdoutPipe()
	if err != nil {
		return nil, err
	}
	pl.Stdin = pipe
	dl.Stderr = io.Discard
	pl.Stderr = io.Discard

	if err := dl.Start(); err != nil {
		return nil, err
	}
	if err := pl.Start(); err != nil {
		dl.Process.Kill() //nolint:errcheck
		dl.Wait()         //nolint:errcheck
		return nil, err
	}

	ended := make(chan struct{})
	go func() { pl.Wait(); close(ended) }() //nolint:errcheck

	select {
	case <-ended:
		dl.Process.Kill() //nolint:errcheck
		dl.Wait()         //nolint:errcheck
		return nil, fmt.Errorf("player crashed on startup")
	case <-time.After(350 * time.Millisecond):
	}
	return &Stream{cmd: pl, dl: dl, ended: ended}, nil
}

func playerValid(path, versionFlag string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, versionFlag)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

func afplayProxy(ytdlp, afplayPath, target string) (*Stream, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// afplay (CoreAudio) can't decode opus/webm, so prefer m4a here; the "web"
	// client in ytExtractorArgs supplies an itag-140 m4a stream.
	raw, err := exec.CommandContext(ctx, ytdlp,
		withYT("--get-url", "-f", "bestaudio[ext=m4a]/bestaudio", "--no-playlist", "--quiet", target)...,
	).Output()
	if err != nil {
		return nil, fmt.Errorf("yt-dlp url: %w", err)
	}
	cdnURL := strings.SplitN(strings.TrimSpace(string(raw)), "\n", 2)[0]
	if cdnURL == "" {
		return nil, fmt.Errorf("no stream URL")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 0}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequestWithContext(r.Context(), "GET", cdnURL, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0")
		if rng := r.Header.Get("Range"); rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body) //nolint:errcheck
	})}
	go srv.Serve(ln) //nolint:errcheck

	port := ln.Addr().(*net.TCPAddr).Port
	cmd := exec.Command(afplayPath, fmt.Sprintf("http://127.0.0.1:%d/audio", port))
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		srv.Close()
		return nil, err
	}
	s := &Stream{cmd: cmd, ended: watchEnded(cmd)}
	go func() { <-s.ended; srv.Close() }()
	return s, nil
}

// ── install hints ─────────────────────────────────────────────────────────────

func ytdlpInstall() string {
	switch runtime.GOOS {
	case "darwin":
		return "  curl -fsSL https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_macos -o /usr/local/bin/yt-dlp && chmod +x /usr/local/bin/yt-dlp"
	case "linux":
		return "  pixeltui doctor --fix   (installs a self-contained yt-dlp into ~/.pixeltui/bin)"
	case "windows":
		return "  winget install yt-dlp"
	default:
		return "  https://github.com/yt-dlp/yt-dlp/releases"
	}
}

func playerInstall() string {
	switch runtime.GOOS {
	case "darwin":
		return "  Install mpv:  make stream-setup  (or: brew install mpv)"
	case "linux":
		return "  sudo apt install mpv   (or ffmpeg for ffplay fallback)"
	case "windows":
		return "  winget install mpv"
	default:
		return "  https://mpv.io/installation/"
	}
}
