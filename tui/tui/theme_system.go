package tui

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── system theme ──────────────────────────────────────────────────────────────
//
// The "system" theme derives the whole palette from the desktop's live theme
// file instead of a hardcoded preset, so pixeltui matches the rest of the
// session -- and keeps matching it: the file is watched, and a theme switch
// recolors the running UI without a restart or a gap in playback.
//
// The file is the Hexarchy palette written on every `hexarchy theme set`:
//
//	$XDG_STATE_HOME/hexarchy/current/theme/colors.toml
//
// It is a flat `key = "#rrggbb"` table. Only the keys below are read, and each
// one falls back to a preset value when absent, so a partial or foreign
// palette still yields a usable theme rather than an error.

// SystemThemeName is the theme name that tracks the desktop palette.
const SystemThemeName = "system"

// systemThemeEnv overrides the palette path (used by tests and for previewing
// a theme that is not the active one).
const systemThemeEnv = "PIXELTUI_THEME_FILE"

// systemPalettePath returns the palette file the system theme follows.
func systemPalettePath() string {
	if p := strings.TrimSpace(os.Getenv(systemThemeEnv)); p != "" {
		return p
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "hexarchy", "current", "theme", "colors.toml")
}

// parsePalette reads the flat `key = "value"` pairs from a colors.toml.
//
// This is deliberately not a TOML parser: the palette is a generated flat
// table of quoted strings, and depending on a real parser for it would pull a
// module in for six lines of work. Anything that is not a quoted scalar
// assignment at the top level is skipped.
func parsePalette(path string) (map[string]string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		key, rest, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		rest = strings.TrimSpace(rest)
		// Only quoted scalars; the closing quote also ends the value, so a
		// trailing `# comment` needs no separate handling.
		if len(rest) < 2 || rest[0] != '"' {
			continue
		}
		val, _, ok := strings.Cut(rest[1:], `"`)
		if !ok || key == "" {
			continue
		}
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no colors found", path)
	}
	return out, nil
}

// ── color helpers ─────────────────────────────────────────────────────────────

type rgb struct{ r, g, b float64 } // components in 0..1

func parseHex(s string) (rgb, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	if len(s) == 3 { // #abc → #aabbcc
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return rgb{}, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return rgb{}, false
	}
	return rgb{
		r: float64((v>>16)&0xff) / 255,
		g: float64((v>>8)&0xff) / 255,
		b: float64(v&0xff) / 255,
	}, true
}

func (c rgb) hex() string {
	q := func(f float64) int { return int(math.Round(math.Max(0, math.Min(1, f)) * 255)) }
	return fmt.Sprintf("#%02x%02x%02x", q(c.r), q(c.g), q(c.b))
}

// hue returns the color's hue in degrees, and whether it is saturated enough
// for that hue to be meaningful.
func (c rgb) hue() (float64, bool) {
	maxc := math.Max(c.r, math.Max(c.g, c.b))
	minc := math.Min(c.r, math.Min(c.g, c.b))
	d := maxc - minc
	if d < 0.04 { // effectively grey: no usable hue
		return 0, false
	}
	var h float64
	switch maxc {
	case c.r:
		h = math.Mod((c.g-c.b)/d, 6)
	case c.g:
		h = (c.b-c.r)/d + 2
	default:
		h = (c.r-c.g)/d + 4
	}
	h *= 60
	if h < 0 {
		h += 360
	}
	return h, true
}

// hueGap is the shortest angular distance between two hues, in degrees.
func hueGap(a, b float64) float64 {
	d := math.Abs(a - b)
	if d > 180 {
		d = 360 - d
	}
	return d
}

// mix blends a toward b by t (0 = a, 1 = b).
func mix(a, b rgb, t float64) rgb {
	return rgb{
		r: a.r + (b.r-a.r)*t,
		g: a.g + (b.g-a.g)*t,
		b: a.b + (b.b-a.b)*t,
	}
}

// ── palette → theme ───────────────────────────────────────────────────────────

// systemTheme is a full palette derived from the desktop theme: the two accents
// a themeDef carries, plus the semantic colors that presets leave fixed.
type systemTheme struct {
	def                           themeDef
	text, dim, green, yellow, red lipgloss.AdaptiveColor
	border, selText               lipgloss.AdaptiveColor
}

// solid returns an AdaptiveColor that is the same in light and dark terminals.
// The palette already encodes which mode it is for, so there is nothing left
// for lipgloss to adapt -- picking per-mode variants here would fight it.
func solid(c rgb) lipgloss.AdaptiveColor {
	h := c.hex()
	return lipgloss.AdaptiveColor{Light: h, Dark: h}
}

// secondaryAccent picks the accent2 companion: the first candidate hue that is
// clearly distinct from the accent, so headers and now-playing never collapse
// into the same color. Falls back to a lightened accent when the palette is
// monochrome or every candidate sits on the accent's hue.
func secondaryAccent(pal map[string]string, accent, fg rgb) rgb {
	ah, accentHued := accent.hue()
	for _, key := range []string{"magenta", "cyan", "blue", "green", "bright_magenta", "bright_cyan"} {
		c, ok := parseHex(pal[key])
		if !ok {
			continue
		}
		ch, hued := c.hue()
		if !hued {
			continue
		}
		if !accentHued || hueGap(ah, ch) >= 30 {
			return c
		}
	}
	return mix(accent, fg, 0.45)
}

// buildSystemTheme maps a Hexarchy palette onto pixeltui's color roles.
func buildSystemTheme(pal map[string]string) systemTheme {
	// pick returns the first key that parses, else the preset fallback.
	pick := func(fallback rgb, keys ...string) rgb {
		for _, k := range keys {
			if c, ok := parseHex(pal[k]); ok {
				return c
			}
		}
		return fallback
	}

	// Fallbacks are mid-contrast neutrals rather than preset accents: if the
	// palette is missing a role entirely, a muted color is a better guess than
	// a purple from an unrelated theme.
	bg := pick(rgb{0.07, 0.07, 0.09}, "background")
	fg := pick(rgb{0.85, 0.85, 0.88}, "foreground")
	brightFg := pick(fg, "bright_foreground", "light_foreground")
	accent := pick(fg, "accent", "blue", "magenta")
	accent2 := secondaryAccent(pal, accent, brightFg)

	// dim/border have no direct equivalent in every palette, so derive them
	// from the background→foreground ramp when the named keys are absent.
	dim := pick(mix(bg, fg, 0.55), "dark_foreground", "muted")
	border := pick(mix(bg, fg, 0.28), "muted", "selection", "lighter_background")

	return systemTheme{
		def: themeDef{
			accent:  solid(accent),
			accent2: solid(accent2),
			grad1:   accent.hex(),
			grad2:   accent2.hex(),
		},
		text:    solid(fg),
		dim:     solid(dim),
		green:   solid(pick(mix(bg, fg, 0.7), "green", "bright_green")),
		yellow:  solid(pick(mix(bg, fg, 0.7), "yellow", "bright_yellow", "orange")),
		red:     solid(pick(mix(bg, fg, 0.7), "red", "bright_red")),
		border:  solid(border),
		selText: solid(brightFg),
	}
}

// loadSystemTheme reads and maps the live desktop palette.
func loadSystemTheme() (systemTheme, error) {
	path := systemPalettePath()
	if path == "" {
		return systemTheme{}, fmt.Errorf("no palette path")
	}
	pal, err := parsePalette(path)
	if err != nil {
		return systemTheme{}, err
	}
	return buildSystemTheme(pal), nil
}

// ── live reload ───────────────────────────────────────────────────────────────

// themeReloadMsg asks the running UI to re-read the desktop palette. Handled in
// Update, so the repaint happens on bubbletea's own loop rather than from the
// watcher goroutine.
type themeReloadMsg struct{}

// paletteStamp identifies a version of the palette file cheaply enough to poll.
func paletteStamp() (string, bool) {
	fi, err := os.Stat(systemPalettePath())
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size()), true
}

var watchOnce sync.Once

// watchSystemTheme nudges the program whenever the desktop palette changes.
//
// Polling rather than inotify: the file is rewritten (not edited in place) by
// `hexarchy theme set`, which replaces the inode and would silently break a
// watch on the original file. A 1s poll of one stat() is cheap, survives the
// replacement, and keeps the binary dependency-free.
//
// A SIGHUP/SIGUSR1 handler is installed alongside it by the caller, so a theme
// hook can make the change instant instead of waiting out the poll.
func watchSystemTheme(p *tea.Program, sig <-chan os.Signal) {
	watchOnce.Do(func() {
		go func() {
			last, _ := paletteStamp()
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					stamp, ok := paletteStamp()
					if !ok || stamp == last {
						continue
					}
					last = stamp
				case <-sig:
					last, _ = paletteStamp()
				}
				p.Send(themeReloadMsg{})
			}
		}()
	})
}
