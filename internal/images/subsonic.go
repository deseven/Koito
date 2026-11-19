package images

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/gabehf/koito/internal/cfg"
	"github.com/gabehf/koito/internal/logger"
	"github.com/gabehf/koito/queue"
	"github.com/google/uuid"
)

type SubsonicClient struct {
	url          string
	userAgent    string
	authParams   string
	requestQueue *queue.RequestQueue

	// artistIndex caches lowercased artist name -> artist image URL, loaded from
	// getArtists. Navidrome's search3 does not reliably return artists, so the
	// index is the dependable way to match artists by name.
	indexOnce    sync.Once
	indexErr     error
	artistsIndex map[string]string
}

type SubsonicAlbumResponse struct {
	SubsonicResponse struct {
		Status        string `json:"status"`
		SearchResult3 struct {
			Album []struct {
				CoverArt string `json:"coverArt"`
				Artist   string `json:"artist"`
				MBID     string `json:"musicBrainzId"`
			} `json:"album"`
			Song []SubsonicSong `json:"song"`
		} `json:"searchResult3"`
	} `json:"subsonic-response"`
}

// SubsonicSong is a song returned by search3. Its album fields are used to find
// cover art for albums that cannot be matched by album title alone, which
// includes releases named after their own track.
type SubsonicSong struct {
	Title    string `json:"title"`
	Album    string `json:"album"`
	Artist   string `json:"artist"`
	CoverArt string `json:"coverArt"`
}

type SubsonicArtistResponse struct {
	SubsonicResponse struct {
		Status        string `json:"status"`
		SearchResult3 struct {
			Artist []struct {
				Name           string `json:"name"`
				ArtistImageUrl string `json:"artistImageUrl"`
			} `json:"artist"`
		} `json:"searchResult3"`
	} `json:"subsonic-response"`
}

type SubsonicArtistsResponse struct {
	SubsonicResponse struct {
		Status  string `json:"status"`
		Artists struct {
			Index []struct {
				Artist []struct {
					Name           string `json:"name"`
					ArtistImageUrl string `json:"artistImageUrl"`
				} `json:"artist"`
			} `json:"index"`
		} `json:"artists"`
	} `json:"subsonic-response"`
}

const (
	subsonicAlbumSearchFmtStr  = "/rest/search3?%s&f=json&query=%s&v=1.13.0&c=koito&artistCount=0&songCount=0&albumCount=10"
	subsonicSongSearchFmtStr   = "/rest/search3?%s&f=json&query=%s&v=1.13.0&c=koito&artistCount=0&albumCount=0&songCount=20"
	subsonicArtistSearchFmtStr = "/rest/search3?%s&f=json&query=%s&v=1.13.0&c=koito&artistCount=10&songCount=0&albumCount=0"
	subsonicArtistsFmtStr      = "/rest/getArtists?%s&f=json&v=1.13.0&c=koito"
	subsonicCoverArtFmtStr     = "/rest/getCoverArt?%s&id=%s&v=1.13.0&c=koito"
)

func NewSubsonicClient() *SubsonicClient {
	ret := new(SubsonicClient)
	ret.url = cfg.SubsonicUrl()
	ret.userAgent = cfg.UserAgent()
	ret.authParams = cfg.SubsonicParams()
	ret.requestQueue = queue.NewRequestQueue(5, 5)
	return ret
}

func (c *SubsonicClient) queue(ctx context.Context, req *http.Request) ([]byte, error) {
	l := logger.FromContext(ctx)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resultChan := c.requestQueue.Enqueue(func(client *http.Client, done chan<- queue.RequestResult) {
		resp, err := client.Do(req)
		if err != nil {
			l.Debug().Err(err).Str("url", req.RequestURI).Msg("Failed to contact ImageSrc")
			done <- queue.RequestResult{Err: err}
			return
		} else if resp.StatusCode >= 300 || resp.StatusCode < 200 {
			err = fmt.Errorf("received non-ok status from Subsonic: %s", resp.Status)
			done <- queue.RequestResult{Body: nil, Err: err}
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		done <- queue.RequestResult{Body: body, Err: err}
	})

	result := <-resultChan
	return result.Body, result.Err
}

func (c *SubsonicClient) getEntity(ctx context.Context, endpoint string, result any) error {
	l := logger.FromContext(ctx)
	url := c.url + endpoint
	l.Debug().Msgf("Sending request to ImageSrc: GET %s", url)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("getEntity: %w", err)
	}
	l.Debug().Msg("Adding ImageSrc request to queue")
	body, err := c.queue(ctx, req)
	if err != nil {
		l.Err(err).Msg("Subsonic request failed")
		return fmt.Errorf("getEntity: %w", err)
	}

	err = json.Unmarshal(body, result)
	if err != nil {
		l.Err(err).Msg("Failed to unmarshal Subsonic response")
		return fmt.Errorf("getEntity: %w", err)
	}

	return nil
}

// coverArtURL builds a URL for a Subsonic cover art id. The auth parameters are
// part of the URL because the image is downloaded and cached by Koito.
func (c *SubsonicClient) coverArtURL(id string) string {
	if id == "" {
		return ""
	}
	return c.url + fmt.Sprintf(subsonicCoverArtFmtStr, c.authParams, url.QueryEscape(id))
}

func (c *SubsonicClient) GetAlbumImage(ctx context.Context, mbid uuid.UUID, artist, album string) (string, error) {
	l := logger.FromContext(ctx)
	l.Debug().Msgf("Finding album image for %s from artist %s", album, artist)

	// 1. release MBID, when Koito knows one
	if mbid != uuid.Nil {
		resp := new(SubsonicAlbumResponse)
		l.Debug().Str("mbid", mbid.String()).Msg("Searching album image by MBID")
		if err := c.getEntity(ctx, fmt.Sprintf(subsonicAlbumSearchFmtStr, c.authParams, url.QueryEscape(mbid.String())), resp); err != nil {
			return "", fmt.Errorf("GetAlbumImage: %v", err)
		}
		if len(resp.SubsonicResponse.SearchResult3.Album) >= 1 {
			if u := c.coverArtURL(resp.SubsonicResponse.SearchResult3.Album[0].CoverArt); u != "" {
				return u, nil
			}
		}
	}

	// 2. album title, matched case-insensitively on the artist
	resp := new(SubsonicAlbumResponse)
	l.Debug().Str("title", album).Str("artist", artist).Msg("Searching album image by title and artist")
	if err := c.getEntity(ctx, fmt.Sprintf(subsonicAlbumSearchFmtStr, c.authParams, url.QueryEscape(album)), resp); err != nil {
		return "", fmt.Errorf("GetAlbumImage: %v", err)
	}
	for _, candidate := range resp.SubsonicResponse.SearchResult3.Album {
		if artistsMatch(candidate.Artist, artist) {
			if u := c.coverArtURL(candidate.CoverArt); u != "" {
				return u, nil
			}
		}
	}

	// 3. songs of that album. This also covers releases named after their own
	// track, where no album of that title exists in the library.
	l.Debug().Str("title", album).Str("artist", artist).Msg("Searching songs to find the album image")
	if err := c.getEntity(ctx, fmt.Sprintf(subsonicSongSearchFmtStr, c.authParams, url.QueryEscape(album)), resp); err != nil {
		return "", fmt.Errorf("GetAlbumImage: %v", err)
	}
	for _, song := range resp.SubsonicResponse.SearchResult3.Song {
		if !artistsMatch(song.Artist, artist) {
			continue
		}
		if strings.EqualFold(song.Album, album) || strings.EqualFold(song.Title, album) {
			if u := c.coverArtURL(song.CoverArt); u != "" {
				l.Debug().Str("album", song.Album).Str("song", song.Title).Msg("GetAlbumImage: found cover art through song search")
				return u, nil
			}
		}
	}

	return "", fmt.Errorf("GetAlbumImage: failed to get album art from subsonic")
}

func (c *SubsonicClient) GetArtistImage(ctx context.Context, mbid uuid.UUID, artist string) (string, error) {
	l := logger.FromContext(ctx)
	l.Debug().Msgf("Finding artist image for %s", artist)

	// 1. artist MBID, when Koito knows one
	if mbid != uuid.Nil {
		resp := new(SubsonicArtistResponse)
		l.Debug().Str("mbid", mbid.String()).Msg("Searching artist image by MBID")
		if err := c.getEntity(ctx, fmt.Sprintf(subsonicArtistSearchFmtStr, c.authParams, url.QueryEscape(mbid.String())), resp); err == nil {
			if len(resp.SubsonicResponse.SearchResult3.Artist) >= 1 {
				if u := c.validArtistImage(resp.SubsonicResponse.SearchResult3.Artist[0].ArtistImageUrl); u != "" {
					return u, nil
				}
			}
		}
	}

	// 2. exact name from the full artist index
	if u := c.indexedArtistImage(ctx, artist); u != "" {
		return u, nil
	}

	// 3. name search, accepting only an exact (case-insensitive) name match
	resp := new(SubsonicArtistResponse)
	l.Debug().Str("artist", artist).Msg("Searching artist image by name")
	if err := c.getEntity(ctx, fmt.Sprintf(subsonicArtistSearchFmtStr, c.authParams, url.QueryEscape(artist)), resp); err != nil {
		return "", fmt.Errorf("GetArtistImage: %v", err)
	}
	for _, candidate := range resp.SubsonicResponse.SearchResult3.Artist {
		if strings.EqualFold(candidate.Name, artist) {
			if u := c.validArtistImage(candidate.ArtistImageUrl); u != "" {
				return u, nil
			}
		}
	}
	return "", fmt.Errorf("GetArtistImage: failed to get artist art")
}

// validArtistImage returns the URL when it actually serves an image. Subsonic
// servers tend to return artist image URLs that 404.
func (c *SubsonicClient) validArtistImage(u string) string {
	if u == "" {
		return ""
	}
	if err := ValidateImageURL(u); err != nil {
		return ""
	}
	return u
}

// indexedArtistImage looks the artist up in the server's A-Z index. The index is
// fetched once and cached for the lifetime of the client.
func (c *SubsonicClient) indexedArtistImage(ctx context.Context, artist string) string {
	l := logger.FromContext(ctx)
	c.indexOnce.Do(func() {
		resp := new(SubsonicArtistsResponse)
		if err := c.getEntity(ctx, fmt.Sprintf(subsonicArtistsFmtStr, c.authParams), resp); err != nil {
			c.indexErr = err
			return
		}
		index := make(map[string]string)
		for _, group := range resp.SubsonicResponse.Artists.Index {
			for _, a := range group.Artist {
				if a.ArtistImageUrl != "" {
					index[strings.ToLower(a.Name)] = a.ArtistImageUrl
				}
			}
		}
		c.artistsIndex = index
		l.Debug().Int("artists", len(index)).Msg("SubsonicClient: loaded artist index")
	})
	if c.indexErr != nil || c.artistsIndex == nil {
		return ""
	}
	if u, ok := c.artistsIndex[strings.ToLower(artist)]; ok {
		return c.validArtistImage(u)
	}
	return ""
}

// artistsMatch reports whether two artist credits refer to the same artist,
// ignoring case. Subsonic servers often use different capitalisation than the
// scrobbled metadata (e.g. "LINGUA IGNOTA" vs "Lingua Ignota").
func artistsMatch(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
