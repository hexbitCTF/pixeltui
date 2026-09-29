package local

// Provider wraps local audio file indexing as a source.Provider.

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dotjarden/pixeltui/tui/engine"
	"github.com/dotjarden/pixeltui/tui/source"
)

// Provider implements source.Provider for local audio files.
type Provider struct {
	dataDir string
	dirs    []string
	art     *Resolver
}

// NewProvider builds a local-file provider. dataDir is used for the metadata
// and album-art caches; dirs are the folders to scan. lastfmKey may be empty,
// in which case cover art is looked for on disk only.
func NewProvider(dataDir string, dirs []string, lastfmKey string) *Provider {
	return &Provider{
		dataDir: dataDir,
		dirs:    dirs,
		art:     NewResolver(dataDir, lastfmKey),
	}
}

func (p *Provider) Key() string   { return "local" }
func (p *Provider) Label() string { return "Server Files" }

// Search implements source.Provider by scanning and matching on artist/title.
func (p *Provider) Search(ctx context.Context, query string, limit int) ([]engine.Candidate, error) {
	all, err := p.scan()
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(query)
	var out []engine.Candidate
	for _, c := range all {
		if strings.Contains(strings.ToLower(c.Track+" "+c.Artist+" "+c.Album), q) {
			out = append(out, c)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// SearchArtists implements source.Provider.
func (p *Provider) SearchArtists(ctx context.Context, query string, limit int) ([]source.ArtistRef, error) {
	return nil, source.ErrNotSupported
}

// SearchAlbums implements source.Provider.
func (p *Provider) SearchAlbums(ctx context.Context, query string, limit int) ([]source.AlbumRef, error) {
	return nil, source.ErrNotSupported
}

// BrowseArtist implements source.Provider.
func (p *Provider) BrowseArtist(ctx context.Context, ref source.ArtistRef) (*source.ArtistPage, error) {
	return nil, source.ErrNotSupported
}

// BrowseAlbum implements source.Provider.
func (p *Provider) BrowseAlbum(ctx context.Context, ref source.AlbumRef) (*source.AlbumPage, error) {
	return nil, source.ErrNotSupported
}

// Charts implements source.Provider.
func (p *Provider) Charts(ctx context.Context, country string, limit int) ([]engine.Candidate, error) {
	return nil, source.ErrNotSupported
}

// Radio implements source.Provider.
func (p *Provider) Radio(ctx context.Context, seedID string, limit int, exclude []string) ([]engine.Candidate, error) {
	return nil, source.ErrNotSupported
}

// StreamURL implements source.Provider. The id is a base64-encoded absolute
// file path.
func (p *Provider) StreamURL(ctx context.Context, id string) (string, error) {
	path, err := base64.URLEncoding.DecodeString(id)
	if err != nil {
		return "", err
	}
	if !p.allowed(string(path)) {
		return "", fmt.Errorf("local file outside configured directories")
	}
	return string(path), nil
}

// ResolveStream implements source.Provider.
func (p *Provider) ResolveStream(ctx context.Context, id string) (string, error) {
	return "", source.ErrNotSupported
}

// ArtURL implements source.Provider. It returns an unresolved local reference
// (see ArtRef): the cover is found and cached on first use, and the HTTP server
// serves it from /api/art?id=lo:… rather than from a URL the client can reach
// directly.
func (p *Provider) ArtURL(ctx context.Context, id string) (string, error) {
	path, err := base64.URLEncoding.DecodeString(id)
	if err != nil {
		return "", err
	}
	if !p.allowed(string(path)) {
		return "", fmt.Errorf("local file outside configured directories")
	}
	return ArtRef(string(path)), nil
}

// TrackInfo implements source.Provider. ArtURL is deliberately left empty:
// the value would be an unresolvable local: reference, and the HTTP surface
// serves local covers through /api/art?id=lo:… instead.
func (p *Provider) TrackInfo(ctx context.Context, id string) (source.TrackInfo, error) {
	path, err := base64.URLEncoding.DecodeString(id)
	if err != nil {
		return source.TrackInfo{}, err
	}
	if !p.allowed(string(path)) {
		return source.TrackInfo{}, fmt.Errorf("local file outside configured directories")
	}
	artist, title, album, dur := metadata(string(path))
	return source.TrackInfo{
		Title:       title,
		Artist:      artist,
		Album:       album,
		DurationSec: dur,
	}, nil
}

// Resolver returns the provider's album-art resolver, so the TUI, the player
// and the server share one cache instead of each re-extracting the same covers.
func (p *Provider) Resolver() *Resolver { return p.art }

// Capabilities implements source.Provider.
func (p *Provider) Capabilities(ctx context.Context, id string) (source.Capabilities, error) {
	return source.Capabilities{
		StartStation: false,
		GoToArtist:   false,
		GoToAlbum:    false,
		Radio:        false,
		Download:     true,
		Lyrics:       true,
		ShareURL:     "",
	}, nil
}

// Liked implements source.Provider. Local files have no server-side like state.
func (p *Provider) Liked(ctx context.Context) ([]engine.Candidate, error) {
	return nil, source.ErrNotSupported
}

// Playlists implements source.Provider. Local files have no server playlists.
func (p *Provider) Playlists(ctx context.Context) ([]string, error) {
	return nil, source.ErrNotSupported
}

// PlaylistTracks implements source.Provider.
func (p *Provider) PlaylistTracks(ctx context.Context, name string) ([]engine.Candidate, error) {
	return nil, source.ErrNotSupported
}

// All returns every track in the configured local directories. This is the
// full catalog scan used for bulk background tasks like audio fingerprinting.
func (p *Provider) All(ctx context.Context) ([]engine.Candidate, error) {
	return p.scan()
}

// scan returns the current candidate list, using the cached index when possible.
func (p *Provider) scan() ([]engine.Candidate, error) {
	if len(p.dirs) == 0 {
		return nil, nil
	}
	if all, ok := Cached(p.dataDir); ok {
		return all, nil
	}
	return Scan(p.dataDir, p.dirs)
}

// allowed reports whether path is under one of the configured directories.
func (p *Provider) allowed(path string) bool {
	for _, d := range p.dirs {
		rel, err := filepath.Rel(d, path)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(rel, "..") && rel != ".." {
			return true
		}
	}
	return false
}

// ExtractCover writes the embedded cover art for a local file to a temp file
// and returns its path. This is a helper for the server art handler; the
// provider does not implement the extraction in ArtURL because it needs a
// filesystem path, not a URL.
func ExtractCover(path string) (string, error) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "pixeltui-cover-")
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "cover.jpg")
	cmd := exec.Command(ff, "-i", path, "-an", "-vcodec", "copy", "-f", "image2", out)
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out, nil
}
