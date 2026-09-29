package subsonic

// Provider wraps a Subsonic/OpenSubsonic client as a source.Provider.

import (
	"context"
	"net/url"

	"github.com/dotjarden/pixeltui/tui/engine"
	"github.com/dotjarden/pixeltui/tui/source"
)

// Provider implements source.Provider for Subsonic-compatible servers.
type Provider struct {
	client *Client
}

// NewProvider wraps an existing Subsonic client.
func NewProvider(c *Client) *Provider {
	return &Provider{client: c}
}

func (p *Provider) Key() string   { return "subsonic" }
func (p *Provider) Label() string { return "Subsonic" }

// Search implements source.Provider.
func (p *Provider) Search(ctx context.Context, query string, limit int) ([]engine.Candidate, error) {
	return p.client.Search(query, limit)
}

// SearchArtists implements source.Provider. Subsonic search3 can return
// artists; for now we return an empty list and rely on the best-match fallback.
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

// StreamURL implements source.Provider. Subsonic returns a direct auth-bearing
// URL for the raw song id.
func (p *Provider) StreamURL(ctx context.Context, id string) (string, error) {
	return p.client.StreamURL(id), nil
}

// ResolveStream implements source.Provider. Subsonic does not need a resolver.
func (p *Provider) ResolveStream(ctx context.Context, id string) (string, error) {
	return "", source.ErrNotSupported
}

// ArtURL implements source.Provider. The cover id is stored in the Subsonic
// song id payload, but the provider only knows the song id here. The server
// proxies art via /api/art?id=su:<songID> and extracts coverArt from the
// original track record; this provider method returns an empty URL so the
// server uses its proxy path instead.
func (p *Provider) ArtURL(ctx context.Context, id string) (string, error) {
	return "", nil
}

// TrackInfo implements source.Provider.
func (p *Provider) TrackInfo(ctx context.Context, id string) (source.TrackInfo, error) {
	return source.TrackInfo{}, source.ErrNotSupported
}

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

// Liked implements source.Provider.
func (p *Provider) Liked(ctx context.Context) ([]engine.Candidate, error) {
	return p.client.Starred()
}

// Playlists implements source.Provider.
func (p *Provider) Playlists(ctx context.Context) ([]string, error) {
	pls, err := p.client.Playlists()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(pls))
	for _, pl := range pls {
		out = append(out, pl.Name)
	}
	return out, nil
}

// PlaylistTracks implements source.Provider.
func (p *Provider) PlaylistTracks(ctx context.Context, name string) ([]engine.Candidate, error) {
	pls, err := p.client.Playlists()
	if err != nil {
		return nil, err
	}
	for _, pl := range pls {
		if pl.Name == name {
			return p.client.PlaylistTracks(pl.ID)
		}
	}
	return nil, nil
}

// StreamAuthURL returns the direct stream URL for a song id (exported for the
// server DTO builder, which needs to extract the raw id from the URL).
func (p *Provider) StreamAuthURL(id string) string {
	return p.client.StreamURL(id)
}

// CoverAuthURL returns the direct cover-art URL for a cover id.
func (p *Provider) CoverAuthURL(coverID string) string {
	return p.client.CoverArtURL(coverID)
}

// QueryParam extracts a query value from a Subsonic auth URL (used by the
// server to pull the raw Subsonic id out of StreamURL strings).
func QueryParam(rawurl, key string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return u.Query().Get(key)
}
