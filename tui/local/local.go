// Package local indexes local audio files in given folders and exposes them
// as engine.Candidate tracks. Local files play directly: the StreamURL is the
// absolute file path, which the player opens as-is (no yt-dlp resolution).
package local

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dotjarden/pixeltui/tui/engine"
)

// Source is the routing tag stamped on every candidate from this package.
const Source = "local"

// audioExts is the set of audio file extensions we include (lowercase).
var audioExts = map[string]bool{
	".mp3": true, ".m4a": true, ".aac": true, ".flac": true,
	".ogg": true, ".opus": true, ".wav": true, ".wma": true,
	".aiff": true, ".aif": true, ".alac": true,
}

// IsAudio reports whether path has a recognized audio extension (case-insensitive).
func IsAudio(path string) bool {
	return audioExts[strings.ToLower(filepath.Ext(path))]
}

// indexVersion invalidates cached entries when the extracted fields change
// (v2 added Album and durations for untagged files).
//
// v3: probe() learned to read ffprobe's stream-level tags, so Ogg/Opus files --
// which is what this program's own downloader produces -- stop being recorded
// with an empty artist and album. Entries written at v2 still carry v2, so each
// one is re-probed once on the next scan and the correct values replace them.
// Without this bump the fix in probe() would never reach an existing library:
// Scan reuses an entry whenever the mtime is unchanged, and downloaded files do
// not change mtime when the reader improves.
const indexVersion = 3

// idxEntry is one cached file record (keyed by absolute path) persisted to
// <dataDir>/local-index.json so re-scans can skip ffprobe for unchanged files.
type idxEntry struct {
	Path   string `json:"path"`
	Artist string `json:"artist"`
	Title  string `json:"title"`
	Album  string `json:"album,omitempty"`
	Dur    int    `json:"dur"`
	Mtime  int64  `json:"mtime"`
	V      int    `json:"v,omitempty"`
}

func indexPath(dataDir string) string { return filepath.Join(dataDir, "local-index.json") }

func loadIndex(dataDir string) map[string]idxEntry {
	m := map[string]idxEntry{}
	b, err := os.ReadFile(indexPath(dataDir))
	if err != nil {
		return m
	}
	var list []idxEntry
	if json.Unmarshal(b, &list) == nil {
		for _, e := range list {
			m[e.Path] = e
		}
	}
	return m
}

func saveIndex(dataDir string, m map[string]idxEntry) {
	b, err := json.Marshal(sortedEntries(m))
	if err != nil {
		return
	}
	tmp := indexPath(dataDir) + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, indexPath(dataDir)) //nolint:errcheck
	}
}

func sortedEntries(m map[string]idxEntry) []idxEntry {
	list := make([]idxEntry, 0, len(m))
	for _, e := range m {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Artist != list[j].Artist {
			return list[i].Artist < list[j].Artist
		}
		return list[i].Title < list[j].Title
	})
	return list
}

func entriesToCandidates(list []idxEntry) []engine.Candidate {
	out := make([]engine.Candidate, len(list))
	for i, e := range list {
		out[i] = engine.Candidate{
			Track: e.Title, Artist: e.Artist, Album: e.Album, DurationSec: e.Dur,
			Source: Source, StreamURL: e.Path, // player opens the path directly
			ArtURL: ArtRef(e.Path), // cover resolved on demand, see art.go
		}
	}
	return out
}

// Cached returns the persisted index as candidates without walking the disk —
// instant. ok is false if no index has been built yet (first-ever open).
func Cached(dataDir string) (out []engine.Candidate, ok bool) {
	m := loadIndex(dataDir)
	if len(m) == 0 {
		return nil, false
	}
	return entriesToCandidates(sortedEntries(m)), true
}

// Scan walks dirs and returns all audio files as candidates (sorted by Artist
// then Track), persisting a metadata index at <dataDir>/local-index.json. Files
// unchanged since the last scan (same mtime) reuse cached metadata, so repeat
// scans skip ffprobe and are near-instant even for large libraries. Unreadable
// dirs/files are skipped (not fatal).
func Scan(dataDir string, dirs []string) ([]engine.Candidate, error) {
	old := loadIndex(dataDir)
	next := make(map[string]idxEntry, len(old))
	for _, dir := range dirs {
		filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || !IsAudio(path) {
				return nil
			}
			abs, aerr := filepath.Abs(path)
			if aerr != nil {
				abs = path
			}
			var mt int64
			if info, ierr := d.Info(); ierr == nil {
				mt = info.ModTime().Unix()
			}
			if e, ok := old[abs]; ok && e.Mtime == mt && e.V == indexVersion {
				next[abs] = e // unchanged → reuse cached metadata (no ffprobe)
				return nil
			}
			artist, title, album, dur := metadata(abs)
			next[abs] = idxEntry{Path: abs, Artist: artist, Title: title, Album: album,
				Dur: dur, Mtime: mt, V: indexVersion}
			return nil
		})
	}
	saveIndex(dataDir, next)
	return entriesToCandidates(sortedEntries(next)), nil
}

// metadata returns (artist, title, album, durationSec) for a file. It prefers
// ffprobe tags; an untagged file still keeps its probed duration, with
// artist/title parsed from the filename.
func metadata(path string) (artist, title, album string, dur int) {
	a, t, al, d, ok := probe(path)
	if ok {
		return a, t, al, d
	}
	fa, ft := fromFilename(path)
	return fa, ft, "", d // d survives even when tags don't
}

// fromFilename derives artist/title from the base name (no extension):
// "Artist - Title" splits on the first " - "; otherwise Artist="" and Track=base.
func fromFilename(path string) (artist, title string) {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if i := strings.Index(base, " - "); i >= 0 {
		return strings.TrimSpace(base[:i]), strings.TrimSpace(base[i+3:])
	}
	return "", base
}

// ffprobeFormat mirrors the slice of ffprobe -show_format JSON we care about.
// Streams are read too because Ogg/Opus -- which is exactly what this program's
// own downloader produces -- carries its Vorbis comments on the stream and
// leaves the format block empty.
type ffprobeFormat struct {
	Format struct {
		Duration string            `json:"duration"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		Tags map[string]string `json:"tags"`
	} `json:"streams"`
}

// tag looks a key up in the format tags first, then the stream tags. ffprobe
// reports the same logical tag at different levels depending on the container:
// an MP3 puts ID3 in format.tags, while an Ogg Opus file puts its Vorbis
// comments in streams[0].tags and reports format.tags as {}. Reading only the
// format level therefore made every freshly downloaded track look untagged --
// no artist, no title, and with a bare filename no artist could be recovered
// from the name either.
func (f *ffprobeFormat) tag(keys ...string) string {
	if v := firstTag(f.Format.Tags, keys...); v != "" {
		return v
	}
	for _, s := range f.Streams {
		if v := firstTag(s.Tags, keys...); v != "" {
			return v
		}
	}
	return ""
}

// probe runs ffprobe (if on PATH) with a short timeout and parses tags.
// ok is false when no title tag was recovered — the caller then derives
// artist/title from the filename — but the probed duration is returned
// regardless so untagged files still show a real length.
func probe(path string) (artist, title, album string, dur int, ok bool) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return "", "", "", 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", path)
	data, err := cmd.Output()
	if err != nil {
		return "", "", "", 0, false
	}
	var f ffprobeFormat
	if json.Unmarshal(data, &f) != nil {
		return "", "", "", 0, false
	}
	title = f.tag("title", "TITLE")
	artist = f.tag("artist", "ARTIST", "album_artist", "ALBUM_ARTIST", "albumartist", "ALBUMARTIST")
	album = f.tag("album", "ALBUM")
	if s := strings.TrimSpace(f.Format.Duration); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			dur = int(v)
		}
	}
	return artist, title, album, dur, strings.TrimSpace(title) != ""
}

// firstTag returns the first non-empty value among the given tag keys.
func firstTag(tags map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(tags[k]); v != "" {
			return v
		}
	}
	return ""
}
