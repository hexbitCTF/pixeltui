package local

// Album art for local files. A local track is just a path on disk, so unlike
// YouTube or Subsonic tracks there is no artwork URL to hand around — the
// cover has to be found. This file resolves one, in escalating cost order:
//
//  1. a sidecar image sitting next to the track (cover.jpg, folder.jpg,
//     <track>.png, …) — free, offline, and what most hand-ripped libraries
//     already look like
//  2. a picture frame embedded in the audio file, pulled out with ffmpeg —
//     covers everything pixeltui downloads, since the download path asks
//     yt-dlp for --embed-thumbnail
//  3. Last.fm album art — one request per album, not per track
//  4. Last.fm artist art — a last resort so a track with no release art still
//     shows something rather than nothing
//
// Results are cached on disk under <dataDir>/artcache so the network is hit
// once per album, ever. Misses are cached too, as an empty marker, so a track
// with no cover doesn't re-run the whole cascade on every play.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/jpeg" // used both to decode and to re-encode cached covers
	"image/png" // used to decode covers and to write generated placeholders
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dotjarden/pixeltui/tui/lastfm"
)

// ArtScheme marks an ArtURL that names a local audio file instead of a
// ready-to-use image. Keeping local tracks in the same "always have an art
// URL" shape as every other source means callers (the now-playing art panel,
// the OS Now Playing widget) need no special-casing: they just load whatever
// URL they were handed, and the cover gets resolved lazily — only for tracks
// someone actually plays, and only once each.
const ArtScheme = "local:"

// ArtRef returns the art URL for a local audio file. The result is a promise
// to find a cover later, not a cover itself.
func ArtRef(path string) string { return ArtScheme + path }

// ErrNoArt means nothing could be found for a local file. Callers treat it as
// "draw no art" rather than as a failure.
var ErrNoArt = errors.New("local: no cover art")

// defaultArtSize is the pixel size of the generated stand-in cover. The TUI
// draws it into a 12x6 cell grid, so this is far more detail than can ever be
// seen; it is square because that is what the renderers assume.
const defaultArtSize = 64

// DefaultArt returns a path to a generated placeholder cover, creating it on
// first use and reusing it afterwards.
//
// Without this, a track with no cover resolved to ErrNoArt, which the TUI draws
// as an empty panel. Worse, every play of such a track re-entered the cascade:
// a sidecar probe, a tag read, an ffmpeg extraction attempt and up to three
// Last.fm requests, under a 12s budget, only to end up drawing nothing. The
// placeholder is cached on disk next to the art cache and the miss marker is
// written alongside it, so that whole path is paid once per album.
//
// The image is derived from the key, so a library of coverless tracks gets
// visually distinct tiles instead of one flat grey square, and it is stable for
// a given track across runs.
func (r *Resolver) defaultArt(key string) string {
	path := filepath.Join(r.cacheDir(), "default-"+key+".png")

	r.mu.Lock()
	_, memoised := r.memo["default:"+key]
	r.mu.Unlock()
	if memoised {
		return path
	}

	if _, err := os.Stat(path); err != nil {
		if err := writeDefaultCover(path, key); err != nil {
			return ""
		}
	}

	r.mu.Lock()
	r.memo["default:"+key] = path
	r.mu.Unlock()

	return path
}

// writeDefaultCover renders a small deterministic placeholder to path.
func writeDefaultCover(path, key string) error {
	// FNV-1a over the key: stable across runs and processes, unlike Go's
	// randomised map hashing, so the same track keeps the same colour.
	sum := fnv.New64a()
	_, _ = sum.Write([]byte(key))
	h := sum.Sum64()

	hue := float64(h%360) / 360.0
	base := hsv(hue, 0.32, 0.30)
	edge := hsv(hue, 0.42, 0.46)

	img := image.NewRGBA(image.Rect(0, 0, defaultArtSize, defaultArtSize))
	// A soft diagonal wash between two hues, with a centred note-ish glyph cut
	// out in a lighter tone so the placeholder reads as "no cover" rather than
	// as broken art.
	for y := 0; y < defaultArtSize; y++ {
		for x := 0; x < defaultArtSize; x++ {
			t := (float64(x) + float64(y)) / (2 * float64(defaultArtSize))
			c := lerpColor(base, edge, t)
			img.SetRGBA(x, y, c)
		}
	}
	drawNoteGlyph(img, hsv(hue, 0.20, 0.78))

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	return png.Encode(f, img)
}

// drawNoteGlyph stamps a blocky eighth-note into img.
func drawNoteGlyph(img *image.RGBA, c color.RGBA) {
	const s = defaultArtSize
	// Stem, flag and head, in unit coordinates scaled to the image.
	stemX0, stemX1 := 0.52, 0.60
	stemY0, stemY1 := 0.16, 0.68
	headCX, headCY, headR := 0.42, 0.72, 0.16

	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			fx, fy := float64(x)/s, float64(y)/s
			on := fx >= stemX0 && fx <= stemX1 && fy >= stemY0 && fy <= stemY1
			if !on && fx >= stemX1 && fx <= 0.86 && fy >= stemY0 && fy <= stemY0+0.16 {
				on = true // the flag
			}
			if !on {
				dx, dy := fx-headCX, fy-headCY
				if dx*dx+dy*dy <= headR*headR {
					on = true
				}
			}
			if on {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

func hsv(h, s, v float64) color.RGBA {
	i := math.Floor(h * 6)
	f := h*6 - i
	p := v * (1 - s)
	q := v * (1 - f*s)
	t := v * (1 - (1-f)*s)
	var r, g, b float64
	switch int(i) % 6 {
	case 0:
		r, g, b = v, t, p
	case 1:
		r, g, b = q, v, p
	case 2:
		r, g, b = p, v, t
	case 3:
		r, g, b = p, q, v
	case 4:
		r, g, b = t, p, v
	default:
		r, g, b = v, p, q
	}
	return color.RGBA{
		R: uint8(math.Round(r * 255)),
		G: uint8(math.Round(g * 255)),
		B: uint8(math.Round(b * 255)),
		A: 255,
	}
}

func lerpColor(a, b color.RGBA, t float64) color.RGBA {
	mix := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*t)) }
	return color.RGBA{R: mix(a.R, b.R), G: mix(a.G, b.G), B: mix(a.B, b.B), A: 255}
}

// process-wide resolver, configured once at startup so the TUI, the player and
// the server share one art cache. Reading art never blocks on configuration in
// practice: SetDefaultResolver runs before any UI is built.
var (
	defaultMu       sync.RWMutex
	defaultResolver *Resolver
)

// SetDefaultResolver installs the process-wide album-art resolver.
func SetDefaultResolver(r *Resolver) {
	defaultMu.Lock()
	defaultResolver = r
	defaultMu.Unlock()
}

// Default returns the process-wide resolver, or nil if none was configured.
// A nil resolver is not fatal: art is optional, and callers fall back to
// loading whatever URL they were handed.
func Default() *Resolver {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultResolver
}

// LoadArt decodes the image behind an art URL, resolving local: references
// through the cascade above. This is the single entry point for art loading in
// the TUI and the player, so neither needs to know which sources produce which
// kind of URL.
func LoadArt(ctx context.Context, artURL string) (image.Image, error) {
	if strings.HasPrefix(artURL, ArtScheme) {
		r := Default()
		if r == nil {
			return nil, ErrNoArt
		}
		return r.Load(ctx, artURL)
	}
	return plainGet(ctx, artURL)
}

// sidecarNames are the cover filenames we look for in a track's own directory
// and in its parent (the album folder), in priority order.
var sidecarNames = []string{
	"cover", "folder", "front", "album", "albumart", "albumartsmall", "thumb", "artwork",
}

// sidecarExts are the image extensions we accept for sidecar art.
var sidecarExts = []string{".jpg", ".jpeg", ".png", ".webp", ".bmp", ".gif"}

const (
	// artTimeout caps a single resolution attempt. Sidecar and embedded art
	// are near-instant; the Last.fm fallbacks are one request each, so this
	// is generous enough to never be the reason a cover fails to appear.
	artTimeout = 12 * time.Second
	// maxArtBytes caps a downloaded cover. Last.fm's largest is a few hundred
	// KB; anything past this is not a cover we drew.
	maxArtBytes = 8 << 20
)

// Resolver finds and caches cover art for local files. It is safe for
// concurrent use — the TUI, the player and the server all share one.
type Resolver struct {
	dataDir string
	lf      *lastfm.Client
	http    *http.Client

	mu   sync.Mutex
	memo map[string]string // "path|mtime" → image path ("" = known to have none)
}

// NewResolver builds a resolver. dataDir is the pixeltui data dir (art is
// cached under <dataDir>/artcache); lastfmKey may be empty, in which case the
// network fallbacks are skipped and only on-disk art is used.
func NewResolver(dataDir, lastfmKey string) *Resolver {
	r := &Resolver{
		dataDir: dataDir,
		http:    &http.Client{Timeout: 8 * time.Second},
		memo:    map[string]string{},
	}
	if lastfmKey != "" {
		r.lf = lastfm.NewClient(lastfmKey)
	}
	return r
}

// Load decodes the image an art URL points at, resolving local art through the
// cascade above. It is the single entry point for the TUI and the player, so
// neither has to know which sources produce which kind of URL.
func (r *Resolver) Load(ctx context.Context, artURL string) (image.Image, error) {
	if strings.HasPrefix(artURL, ArtScheme) {
		path, err := r.Resolve(ctx, strings.TrimPrefix(artURL, ArtScheme))
		if err != nil {
			return nil, err
		}
		return decodeFile(path)
	}
	return r.fetch(ctx, artURL)
}

// Resolve returns a path to a local image file for the given audio file, or
// ErrNoArt. The returned path is stable and may live in the shared art cache,
// so callers must not modify it.
func (r *Resolver) Resolve(ctx context.Context, audioPath string) (string, error) {
	if audioPath == "" {
		return "", ErrNoArt
	}
	key := r.memoKey(audioPath)

	r.mu.Lock()
	if p, ok := r.memo[key]; ok {
		r.mu.Unlock()
		if p == "" {
			// Remembered miss: hand back the generated cover without
			// touching the network or the audio file again.
			return r.defaultArt(key), nil
		}
		return p, nil
	}
	r.mu.Unlock()

	path, err := r.resolveUncached(ctx, audioPath)

	r.mu.Lock()
	r.memo[key] = path
	r.mu.Unlock()

	if err != nil {
		return "", err
	}
	return path, nil
}

// resolveUncached runs the cascade. It returns ("", err) on a miss.
func (r *Resolver) resolveUncached(ctx context.Context, audioPath string) (string, error) {
	// 1. Sidecar art: a cover file next to the track or in its album folder.
	// No cache entry — the file is already on disk and already the cheapest
	// possible answer, so pointing straight at it keeps it live: drop a new
	// cover.jpg in the folder and the next play picks it up.
	if p := sidecarArt(audioPath); p != "" {
		return p, nil
	}

	// Everything below is expensive enough to be worth remembering across runs.
	// metadata() already falls back to parsing "Artist - Title" out of the
	// filename, which is often the only tag a hand-downloaded file carries.
	artist, title, album, _ := metadata(audioPath)
	albumKey := r.albumKey(artist, album, audioPath)
	if albumKey == "" {
		// Nothing to key a lookup on -- a bare file with no tags and no
		// "Artist - Title" in its name. There is no cascade left to run, so go
		// straight to the generated cover. Keying the memo on the path keeps a
		// later play of the same file from repeating even this much work.
		key := r.memoKey(audioPath)
		r.mu.Lock()
		r.memo[key] = ""
		r.mu.Unlock()
		return r.defaultArt(key), nil
	}
	if p, ok := r.cacheLookup(albumKey); ok {
		if p == "" {
			return "", ErrNoArt
		}
		return p, nil
	}

	ctx, cancel := context.WithTimeout(ctx, artTimeout)
	defer cancel()

	// 2. Embedded picture frame (ID3 APIC, FLAC/Vorbis picture, MP4 cover, …).
	if p, err := r.storeExtracted(ctx, albumKey, audioPath); err == nil {
		return p, nil
	}

	// 3/4. Last.fm, per album then per track then per artist. Each is one
	// request, and the album hit is what normally lands.
	for _, url := range r.lastfmArt(ctx, artist, album, title) {
		if p, err := r.storeDownloaded(ctx, albumKey, url); err == nil {
			return p, nil
		}
	}

	// Nothing anywhere. Rather than leaving the panel empty and, on every
	// later play of this album, re-running the whole cascade to rediscover that,
	// fall back to a generated cover. It renders through exactly the same path
	// as real art, so it shows up as an image in the now-playing panel and on
	// the OS Now Playing widget like any other cover.
	r.cacheStore(albumKey, "") // remember the miss
	return r.defaultArt(albumKey), nil
}

// lastfmArt returns candidate cover URLs from Last.fm, cheapest/best first.
// It yields nothing when no key is configured or the lookup fails.
//
// The checks are on *found art*, not on requests made: Last.fm answers plenty
// of lookups successfully with no image attached, and those must fall through
// to the next source rather than count as a hit. (Appending the empty string
// and testing the slice length is what made a successful-but-artless answer
// look like a hit and silently kill the artist fallback behind it.)
func (r *Resolver) lastfmArt(ctx context.Context, artist, album, title string) []string {
	if r.lf == nil {
		return nil
	}
	var out []string
	if album != "" {
		if a, err := r.lf.GetAlbumInfo(artist, album); err == nil && a.ArtURL != "" {
			out = append(out, a.ArtURL)
		}
	}
	if len(out) == 0 && title != "" {
		// A track without an album tag still resolves via track.getInfo, which
		// carries the release art.
		if t, err := r.lf.GetTrackInfo(artist, title); err == nil && t.ArtURL != "" {
			out = append(out, t.ArtURL)
		}
	}
	if len(out) == 0 && artist != "" {
		if a, err := r.lf.GetArtistInfo(artist); err == nil && a.ArtURL != "" {
			out = append(out, a.ArtURL)
		}
	}
	return out
}

// albumKey identifies the art lookup for a track. It folds in the file's
// mtime so that replacing or re-tagging a file invalidates its cached art
// instead of pinning the old answer forever. It returns "" when the track has
// neither an artist nor an album, since there would be nothing to look up.
func (r *Resolver) albumKey(artist, album, audioPath string) string {
	if artist == "" && album == "" {
		return ""
	}
	var mt int64
	if st, err := os.Stat(audioPath); err == nil {
		mt = st.ModTime().Unix()
	}
	sum := sha256.Sum256([]byte(strings.ToLower(artist) + "\x00" +
		strings.ToLower(album) + "\x00" + fmt.Sprint(mt)))
	return hex.EncodeToString(sum[:])
}

// memoKey identifies one audio file for the in-process memo.
func (r *Resolver) memoKey(audioPath string) string {
	var mt int64
	if st, err := os.Stat(audioPath); err == nil {
		mt = st.ModTime().Unix()
	}
	return audioPath + "|" + fmt.Sprint(mt)
}

func (r *Resolver) cacheDir() string { return filepath.Join(r.dataDir, "artcache") }

// cacheLookup returns the cached image path for key. ok is false when nothing
// has been cached yet; a cached empty path is a recorded miss.
func (r *Resolver) cacheLookup(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	dir := r.cacheDir()
	if _, err := os.Stat(filepath.Join(dir, key+".none")); err == nil {
		return "", true
	}
	p := filepath.Join(dir, key+".jpg")
	if st, err := os.Stat(p); err == nil && st.Size() > 0 {
		return p, true
	}
	return "", false
}

func (r *Resolver) cacheStore(key, path string) {
	if key == "" {
		return
	}
	dir := r.cacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	if path == "" {
		os.WriteFile(filepath.Join(dir, key+".none"), nil, 0o644) //nolint:errcheck
	}
}

// storeExtracted pulls an embedded picture frame out of the audio file with
// ffmpeg and caches the result as a JPEG.
func (r *Resolver) storeExtracted(ctx context.Context, key, audioPath string) (string, error) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", err
	}
	dir := r.cacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, key+".jpg")
	tmp := dst + ".part"

	// -frames:v 1 takes the first (usually only) picture frame; -q:v 4 keeps
	// it visually lossless at thumbnail sizes. ffmpeg exits non-zero when the
	// file has no video stream at all, which is the common case.
	cmd := exec.CommandContext(ctx, ff,
		"-hide_banner", "-loglevel", "error",
		"-i", audioPath, "-an", "-frames:v", "1", "-q:v", "4", "-y", tmp)
	if err := cmd.Run(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return "", err
	}
	if st, err := os.Stat(tmp); err != nil || st.Size() == 0 {
		os.Remove(tmp) //nolint:errcheck
		return "", ErrNoArt
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return "", err
	}
	return dst, nil
}

// storeDownloaded fetches a cover URL and caches it as a JPEG. Last.fm serves
// whatever format the original upload was in — PNG under a .jpg URL often — so
// the bytes are decoded and re-encoded rather than trusted to match the name
// we give them or to be something a decoder accepts.
func (r *Resolver) storeDownloaded(ctx context.Context, key, url string) (string, error) {
	if key == "" || url == "" {
		return "", ErrNoArt
	}
	dir := r.cacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, key+".jpg")
	tmp := dst + ".part"
	if err := r.downloadAsJPEG(ctx, url, tmp); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return "", err
	}
	if st, err := os.Stat(tmp); err != nil || st.Size() == 0 {
		os.Remove(tmp) //nolint:errcheck
		return "", ErrNoArt
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return "", err
	}
	return dst, nil
}

// downloadAsJPEG writes the image at url to path, re-encoded as JPEG so the
// cached file's format always matches its .jpg extension.
func (r *Resolver) downloadAsJPEG(ctx context.Context, url, path string) error {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("local: unsupported art URL %q", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("local: art download: %s", resp.Status)
	}
	img, _, err := image.Decode(io.LimitReader(resp.Body, maxArtBytes))
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return jpeg.Encode(f, img, &jpeg.Options{Quality: 92})
}

// fetch downloads and decodes a remote image.
func (r *Resolver) fetch(ctx context.Context, url string) (image.Image, error) {
	return plainGet(ctx, url)
}

// plainGet downloads and decodes a remote image, capped at maxArtBytes.
func plainGet(ctx context.Context, url string) (image.Image, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("local: unsupported art URL %q", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("local: art download: %s", resp.Status)
	}
	img, _, err := image.Decode(io.LimitReader(resp.Body, maxArtBytes))
	return img, err
}

// decodeFile decodes an image from disk.
func decodeFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

// sidecarArt looks for a cover image beside the track and in its parent
// folder. This is the offline path: no network, no ffmpeg, no cache.
func sidecarArt(audioPath string) string {
	dir := filepath.Dir(audioPath)
	base := strings.TrimSuffix(filepath.Base(audioPath), filepath.Ext(audioPath))
	parents := []string{dir}
	if p := filepath.Dir(dir); p != dir {
		parents = append(parents, p)
	}
	for _, d := range parents {
		// Per-track art first — "Temperature.png" beats a shared folder
		// cover for the one file it belongs to.
		for _, ext := range sidecarExts {
			for _, name := range []string{base, base + "-cover", base + "_cover"} {
				if p := filepath.Join(d, name+ext); isImage(p) {
					return p
				}
			}
		}
		for _, ext := range sidecarExts {
			for _, name := range sidecarNames {
				if p := filepath.Join(d, name+ext); isImage(p) {
					return p
				}
			}
		}
	}
	return ""
}

func isImage(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir() && st.Size() > 0
}

// DefaultArtForTest exposes the generated cover to other packages' tests.
func (r *Resolver) DefaultArtForTest(key string) string { return r.defaultArt(key) }
