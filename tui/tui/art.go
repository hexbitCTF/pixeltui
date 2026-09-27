package tui

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

// fetchImage downloads and decodes an image URL (jpeg/png).
func fetchImage(url string) (image.Image, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	img, _, err := image.Decode(resp.Body)
	return img, err
}

// renderArt renders artURL into `rows` strings of `cols` visual characters,
// suitable for dropping straight into the lipgloss layout that reserves that
// cell grid. Terminals that advertise the kitty graphics protocol get a real
// raster image (via Unicode placeholders); everything else falls back to the
// half-block ANSI renderer.
func renderArt(artURL string, cols, rows int) ([]string, error) {
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
func renderArtKitty(artURL string, cols, rows int) ([]string, error) {
	src, err := fetchImage(artURL)
	if err != nil {
		return nil, err
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
