package tui

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png" // register the PNG decoder for covers saved as PNG
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"golang.org/x/image/draw"

	"github.com/dotjarden/pixeltui/tui/local"
)

// fetchImage resolves an art URL to a decoded image. Remote covers are
// downloaded; local files go through the local album-art cascade, so a track
// from ~/Music renders the same panel a YouTube track does.
func fetchImage(url string) (image.Image, error) {
	ctx, cancel := context.WithTimeout(context.Background(), artFetchTimeout)
	defer cancel()
	return local.LoadArt(ctx, url)
}

// artFetchTimeout bounds one art load. Local resolution can shell out to
// ffmpeg and talk to Last.fm, so it gets a more generous ceiling than a plain
// image GET — past it, we simply draw no art rather than stall the UI.
const artFetchTimeout = 15 * time.Second

// artCache memoises rendered art, keyed by URL and the cell grid it was
// rendered for.
//
// Rendering is the expensive part: it decodes the source image (often a
// multi-megapixel cover), resamples it, and on the kitty path re-encodes the
// whole thing to PNG before base64-ing it into a control sequence. Without this
// every track change paid that in full, even when consecutive tracks shared one
// album cover, which is what made navigation feel heavy in a library with art.
//
// Entries are the rendered lines, so a repeat play of the same cover is a map
// lookup. The map is bounded and evicts the oldest entry once it is full, which
// keeps memory flat for big libraries without ever serving a stale render for
// a grid size that no longer matches.
var artCache = struct {
	mu    sync.Mutex
	m     map[string][]string
	order []string
}{m: map[string][]string{}}

const artCacheMax = 24

func artCacheKey(url string, cols, rows int) string {
	return url + "\x00" + strconv.Itoa(cols) + "x" + strconv.Itoa(rows)
}

func artCacheGet(key string) ([]string, bool) {
	artCache.mu.Lock()
	defer artCache.mu.Unlock()
	lines, ok := artCache.m[key]
	return lines, ok
}

func artCachePut(key string, lines []string) {
	artCache.mu.Lock()
	defer artCache.mu.Unlock()
	if _, exists := artCache.m[key]; !exists {
		artCache.order = append(artCache.order, key)
		for len(artCache.order) > artCacheMax {
			delete(artCache.m, artCache.order[0])
			artCache.order = artCache.order[1:]
		}
	}
	artCache.m[key] = lines
}

// artCacheDrop forgets one entry. Called when a track changes so a cover that
// failed to load does not stay cached as a failure.
func artCacheDrop(url string, cols, rows int) {
	artCache.mu.Lock()
	defer artCache.mu.Unlock()
	key := artCacheKey(url, cols, rows)
	if _, ok := artCache.m[key]; !ok {
		return
	}
	delete(artCache.m, key)
	for i, k := range artCache.order {
		if k == key {
			artCache.order = append(artCache.order[:i], artCache.order[i+1:]...)
			break
		}
	}
}

// renderArt renders artURL into `rows` strings of `cols` visual characters,
// suitable for dropping straight into the lipgloss layout that reserves that
// cell grid. Terminals that advertise the kitty graphics protocol get a real
// raster image (via Unicode placeholders); everything else falls back to the
// half-block ANSI renderer.
//
// Results are memoised: the returned lines are the same slice the cache holds,
// so callers must treat them as read-only.
func renderArt(artURL string, cols, rows int) ([]string, error) {
	key := artCacheKey(artURL, cols, rows)
	if lines, ok := artCacheGet(key); ok {
		return lines, nil
	}

	lines, err := renderArtUncached(artURL, cols, rows)
	if err != nil {
		artCacheDrop(artURL, cols, rows)
		return nil, err
	}
	artCachePut(key, lines)
	return lines, nil
}

func renderArtUncached(artURL string, cols, rows int) ([]string, error) {
	if supportsKittyGraphics() {
		if lines, err := renderArtKitty(artURL, cols, rows); err == nil {
			return lines, nil
		}
		// fall through to the half-block renderer on any kitty-path error
		// (network hiccup, decode failure, etc.)
	}

	src, err := fetchImage(artURL)
	if err != nil {
		return nil, err
	}

	// Resize to cols × (rows*2) pixels using nearest-neighbor.
	// Each character cell represents 2 vertical pixels (▀ upper half block).
	img := resizeNearest(src, cols, rows*2)

	lines := make([]string, rows)
	for row := 0; row < rows; row++ {
		var sb strings.Builder
		for col := 0; col < cols; col++ {
			top := rgbaAt(img, col, row*2)
			bot := rgbaAt(img, col, row*2+1)
			sb.WriteString(ansiBlock(top, bot))
		}
		lines[row] = sb.String()
	}
	return lines, nil
}

// kittyArtImageID is a single reused image slot: each new track deletes
// whatever art was last stored there before transmitting the next one, so
// browsing many tracks doesn't leak images into the terminal's cache.
const kittyArtImageID = 1001

// supportsKittyGraphics reports whether the current terminal advertises the
// kitty graphics protocol (kitty itself, and the other terminals — WezTerm,
// Ghostty — that implement the same protocol).
func supportsKittyGraphics() bool {
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return true
	}
	if strings.Contains(os.Getenv("TERM"), "kitty") {
		return true
	}
	switch os.Getenv("TERM_PROGRAM") {
	case "WezTerm", "ghostty":
		return true
	}
	return false
}

// renderArtKitty renders artURL as a real raster image using the kitty
// graphics protocol's Unicode placeholder mode: the image is transmitted
// once, then displayed by printing `cols`×`rows` placeholder runes (each
// tagged with a row/column diacritic and a foreground color that encodes the
// image id). Those placeholder cells behave like ordinary text as far as
// lipgloss/bubbletea layout and redraws are concerned, but kitty renders the
// actual image over them — so this drops straight into the same grid the
// half-block renderer occupies, at full resolution instead of one cell per
// two source pixels.
// artPixelScale is how many device pixels we ask for per cell on each axis
// before handing the image to the terminal.
//
// The now-playing panel is a tiny grid (artCols x artRows cells), so the
// terminal is going to scale whatever we give it up to the panel size anyway.
// Transmitting a full-resolution cover instead means a 1000x1000 JPEG becomes
// megabytes of base64 in a single terminal line: the TUI has to build it, the
// renderer has to diff it, and the emulator has to parse it -- which is what
// made the whole program crawl and misbehave while navigating.
//
// 16px/cell is comfortably above a 2x HiDPI cell (a typical cell is ~8x16
// device px, so ~8x32 at 2x), so the picture still looks sharp; what is removed
// is detail the panel could never show.
const artPixelScale = 16

// artMaxPixels caps the transmitted area regardless of the grid, so a huge
// source cannot produce an unreasonable payload.
const artMaxPixels = 1 << 20 // 1024x1024

// artTargetSize returns the pixel size to transmit for a cols x rows cell grid.
func artTargetSize(src image.Image, cols, rows int) (int, int) {
	b := src.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return 0, 0
	}

	w := cols * artPixelScale
	h := rows * artPixelScale * 2 // cells are roughly twice as tall as wide

	// Never upscale: a source smaller than the target is already cheap.
	if w > b.Dx() {
		w = b.Dx()
	}
	if h > b.Dy() {
		h = b.Dy()
	}

	// Keep the total within the cap while preserving aspect ratio.
	if w*h > artMaxPixels {
		scale := math.Sqrt(float64(artMaxPixels) / float64(w*h))
		w = int(float64(w) * scale)
		h = int(float64(h) * scale)
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return w, h
}

func renderArtKitty(artURL string, cols, rows int) ([]string, error) {
	src, err := fetchImage(artURL)
	if err != nil {
		return nil, err
	}

	// Shrink to what the panel can actually show before encoding. Without this
	// the full cover is base64'd into the output.
	if w, h := artTargetSize(src, cols, rows); w < src.Bounds().Dx() || h < src.Bounds().Dy() {
		src = resizeQuality(src, w, h)
	}

	var out strings.Builder

	del := kitty.Options{
		Action:          kitty.Delete,
		Delete:          kitty.DeleteID,
		ID:              kittyArtImageID,
		DeleteResources: true,
		Quite:           2,
	}
	out.WriteString(ansi.KittyGraphics(nil, del.Options()...))

	tx := &kitty.Options{
		Action:       kitty.Transmit,
		Format:       kitty.PNG,
		ID:           kittyArtImageID,
		Transmission: kitty.Direct,
		Chunk:        true,
		Quite:        2,
	}
	var payload bytes.Buffer
	if err := kitty.EncodeGraphics(&payload, src, tx); err != nil {
		return nil, err
	}
	out.WriteString(payload.String())

	put := kitty.Options{
		Action:           kitty.Put,
		ID:               kittyArtImageID,
		PlacementID:      1,
		VirtualPlacement: true,
		Columns:          cols,
		Rows:             rows,
		Quite:            2,
	}
	out.WriteString(ansi.KittyGraphics(nil, put.Options()...))

	fg := fmt.Sprintf("\x1b[38;2;%d;%d;%dm",
		(kittyArtImageID>>16)&0xFF, (kittyArtImageID>>8)&0xFF, kittyArtImageID&0xFF)

	lines := make([]string, rows)
	for r := 0; r < rows; r++ {
		var sb strings.Builder
		if r == 0 {
			// The transmit/put control sequences carry no visible width; they
			// only need to reach the terminal once, so they ride along on
			// the first line rather than needing a row of their own.
			sb.WriteString(out.String())
		}
		sb.WriteString(fg)
		for c := 0; c < cols; c++ {
			sb.WriteRune(kitty.Placeholder)
			sb.WriteRune(kitty.Diacritic(r))
			sb.WriteRune(kitty.Diacritic(c))
		}
		sb.WriteString("\x1b[0m")
		lines[r] = sb.String()
	}
	return lines, nil
}

// ── image helpers ─────────────────────────────────────────────────────────────

// resizeNearest returns a new RGBA image of size (w, h) using
// nearest-neighbor sampling from src. No external packages required.
// resizeQuality scales src to w x h with a smooth filter. The kitty path shows
// the result scaled up into a small panel, so nearest-neighbour downscaling
// from a multi-megapixel source throws away exactly the detail that survives
// the upscale -- hence CatmullRom rather than the cheap sampler below.
func resizeQuality(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if w < 1 || h < 1 {
		return dst
	}
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	return dst
}

func resizeNearest(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy := b.Min.Y + y*b.Dy()/h
		for x := 0; x < w; x++ {
			sx := b.Min.X + x*b.Dx()/w
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

// rgbaAt reads a pixel from the image and returns it as color.RGBA.
func rgbaAt(img image.Image, x, y int) color.RGBA {
	c := img.At(x, y)
	r, g, b, _ := c.RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8)}
}

// ansiBlock returns a single "▀" character styled with 24-bit ANSI colors:
// fg = top pixel (upper half block uses foreground color),
// bg = bot pixel (lower half block is background).
func ansiBlock(top, bot color.RGBA) string {
	return fmt.Sprintf(
		"\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀\x1b[0m",
		top.R, top.G, top.B,
		bot.R, bot.G, bot.B,
	)
}
