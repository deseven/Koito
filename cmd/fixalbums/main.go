// Command fixalbums repairs releases that hold a track whose title was used as
// its own release title. The Last.fm importer substitutes the track title when
// an export entry has no album:
//
//	album := track.Album.Text
//	if album == "" { album = track.Name }
//
// which leaves pseudo-albums behind: a release "Falling" containing the single
// track "Falling". Two passes are available:
//
// Local pass (default, offline): for every pseudo track, another track of the
// same artist + title that sits on a plausible album is looked up in Koito's own
// data and the two tracks are merged, so all listens move to the real album.
//
// Navidrome pass (-navidrome): the album of every pseudo track is looked up on
// the configured Subsonic/Navidrome server. The rules are:
//
//  1. If the track's album on the server is named after the track itself, and
//     that album holds more than one track, the album is legitimate and left
//     as-is.
//  2. Otherwise, if the server knows another album for the track, the track is
//     moved to that album (created locally when missing).
//  3. If the server knows nothing about the track, and there is no album of
//     that name with several tracks, the track is moved to "Unknown Album".
//
// Tracks are merged instead of moved when the target album already holds a
// track with the same title, so a song never appears twice on one album. The
// tool is safe to re-run. By default it only reports what it would do; pass
// -apply to write.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/gabehf/koito/internal/cfg"
	"github.com/gabehf/koito/internal/db"
	"github.com/gabehf/koito/internal/db/sqlite"
	"github.com/gabehf/koito/internal/images"
	"github.com/gabehf/koito/internal/logger"
)

// unknownAlbum is the release title used for tracks whose album is unknown.
const unknownAlbum = "Unknown Album"

// pseudoTrack is a track whose release is named after the track itself.
type pseudoTrack struct {
	trackID   int32
	track     string
	artistID  int32
	artist    string
	releaseID int32
}

// candidate is another track with the same artist and title, on another release.
type candidate struct {
	trackID   int32
	releaseID int32
	title     string
	tracks    int
	listens   int
}

// plausible reports whether the candidate's release looks like a real album
// rather than another track-titled pseudo-album. A release with more than one
// track is certainly an album; a single-track release counts only when its track
// has listens, i.e. the export did know an album for those scrobbles.
func (c candidate) plausible() bool {
	return c.tracks > 1 || c.listens > 0
}

const pseudoTrackQuery = `
	SELECT t.id, ta.alias, a.id, aa.alias, r.id
	FROM releases r
	JOIN release_aliases ra ON ra.release_id = r.id AND ra.is_primary = 1
	JOIN tracks t ON t.release_id = r.id
	JOIN track_aliases ta ON ta.track_id = t.id AND ta.is_primary = 1
	JOIN artist_tracks at2 ON at2.track_id = t.id
	JOIN artists a ON a.id = at2.artist_id
	JOIN artist_aliases aa ON aa.artist_id = a.id AND aa.is_primary = 1
	WHERE ta.alias = ra.alias
	  AND r.id IN (SELECT release_id FROM tracks GROUP BY release_id HAVING COUNT(*) = 1)
	  AND t.id > ?
	ORDER BY t.id
	LIMIT 200`

const candidateQuery = `
	SELECT t2.id, t2.release_id, ra2.alias,
	       (SELECT COUNT(*) FROM tracks t3 WHERE t3.release_id = t2.release_id),
	       (SELECT COUNT(*) FROM listens l WHERE l.track_id = t2.id)
	FROM artist_tracks at2
	JOIN tracks t2 ON t2.id = at2.track_id
	JOIN track_aliases ta2 ON ta2.track_id = t2.id AND ta2.is_primary = 1
	JOIN release_aliases ra2 ON ra2.release_id = t2.release_id AND ra2.is_primary = 1
	WHERE at2.artist_id = ?1
	  AND ta2.alias = ?2
	  AND t2.id <> ?3
	  AND t2.release_id <> ?4`

func main() {
	apply := flag.Bool("apply", false, "write changes to the database (default is a dry run)")
	verbose := flag.Bool("v", false, "also print cases that are left as they are")
	limit := flag.Int("limit", 0, "process at most N pseudo tracks (0 = no limit)")
	viaNavidrome := flag.Bool("navidrome", false, "resolve albums against the configured Subsonic/Navidrome server, falling back to \""+unknownAlbum+"\"")
	flag.Parse()

	if err := cfg.Load(os.Getenv, "fixalbums"); err != nil {
		fmt.Fprintf(os.Stderr, "fixalbums: %v\n", err)
		os.Exit(1)
	}
	ctx := logger.NewContext(logger.Get())

	store, err := sqlite.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixalbums: %v\n", err)
		os.Exit(1)
	}
	defer store.Close(context.Background())

	if *viaNavidrome {
		if !cfg.SubsonicEnabled() {
			fmt.Fprintf(os.Stderr, "fixalbums: -navidrome requires %s and %s to be set\n",
				cfg.SUBSONIC_URL_ENV, cfg.SUBSONIC_PARAMS_ENV)
			os.Exit(1)
		}
		if err := resolveFromNavidrome(ctx, store, *apply, *verbose, *limit); err != nil {
			fmt.Fprintf(os.Stderr, "fixalbums: %v\n", err)
			os.Exit(1)
		}
	} else {
		if err := fixTracks(ctx, store, *apply, *verbose, *limit); err != nil {
			fmt.Fprintf(os.Stderr, "fixalbums: %v\n", err)
			os.Exit(1)
		}
	}

	if !*apply {
		fmt.Println("\ndry run: nothing was written. re-run with -apply to apply these changes")
	}
}

// forEachPseudoTrack pages through the pseudo tracks and calls fn for each one.
// A track with several artists is only visited once.
func forEachPseudoTrack(store *sqlite.Sqlite, fn func(pseudoTrack) (stop bool, err error)) error {
	lastID := int32(0)
	handled := make(map[int32]bool)
	for {
		rows, err := store.Query(pseudoTrackQuery, lastID)
		if err != nil {
			return fmt.Errorf("query pseudo tracks: %w", err)
		}
		var batch []pseudoTrack
		for rows.Next() {
			var p pseudoTrack
			if err := rows.Scan(&p.trackID, &p.track, &p.artistID, &p.artist, &p.releaseID); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, p := range batch {
			lastID = p.trackID
			if handled[p.trackID] {
				continue
			}
			handled[p.trackID] = true
			stop, err := fn(p)
			if err != nil || stop {
				return err
			}
		}
	}
}

// ---- local pass ----------------------------------------------------------

func fixTracks(ctx context.Context, store *sqlite.Sqlite, apply, verbose bool, limit int) error {
	moved, skipped := 0, 0
	err := forEachPseudoTrack(store, func(p pseudoTrack) (bool, error) {
		if limit > 0 && moved+skipped >= limit {
			return true, nil
		}
		cands, err := candidatesFor(store, p)
		if err != nil {
			return false, err
		}
		target, ok, reason := choose(cands)
		if !ok {
			skipped++
			if verbose {
				fmt.Printf("keep   %-28s %-40s : %s\n", truncate(p.artist, 28), truncate(p.track, 40), reason)
			}
			return false, nil
		}
		fmt.Printf("merge  %-28s %-40s : %q -> %q\n", truncate(p.artist, 28), truncate(p.track, 40), truncate(p.track, 40), truncate(target.title, 40))
		if apply {
			if err := store.MergeTracks(ctx, p.trackID, target.trackID); err != nil {
				return false, err
			}
		}
		moved++
		return false, nil
	})
	fmt.Printf("\ntracks merged into a real album: %d\n", moved)
	fmt.Printf("tracks left untouched:           %d\n", skipped)
	return err
}

func candidatesFor(store *sqlite.Sqlite, p pseudoTrack) ([]candidate, error) {
	rows, err := store.Query(candidateQuery, p.artistID, p.track, p.trackID, p.releaseID)
	if err != nil {
		return nil, fmt.Errorf("query candidates: %w", err)
	}
	defer rows.Close()
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.trackID, &c.releaseID, &c.title, &c.tracks, &c.listens); err != nil {
			return nil, err
		}
		cands = append(cands, c)
	}
	return cands, rows.Err()
}

// choose picks the merge target. Nothing is changed when there is no candidate,
// when no candidate sits on a plausible album, or when candidates sit on several
// plausible albums (ambiguous). Within the chosen album the track with the most
// listens is used as target.
func choose(cands []candidate) (candidate, bool, string) {
	var plausible []candidate
	for _, c := range cands {
		if c.plausible() {
			plausible = append(plausible, c)
		}
	}
	if len(plausible) == 0 {
		return candidate{}, false, "no album found for this artist + track"
	}

	byRelease := make(map[int32][]candidate)
	for _, c := range plausible {
		byRelease[c.releaseID] = append(byRelease[c.releaseID], c)
	}
	if len(byRelease) > 1 {
		names := make([]string, 0, len(byRelease))
		for _, group := range byRelease {
			names = append(names, group[0].title)
		}
		sort.Strings(names)
		return candidate{}, false, fmt.Sprintf("%d plausible albums (%q vs %q), left as-is", len(names), names[0], names[1])
	}

	for _, group := range byRelease {
		sort.Slice(group, func(i, j int) bool {
			if group[i].listens != group[j].listens {
				return group[i].listens > group[j].listens
			}
			return group[i].trackID < group[j].trackID
		})
		return group[0], true, ""
	}
	return candidate{}, false, "no album found for this artist + track"
}

// ---- navidrome pass ------------------------------------------------------

type navidromeResolver struct {
	client *images.SubsonicClient
	// songCache/albumCache hold search results per query; many pseudo tracks
	// share a title or album, so this saves a lot of requests.
	songCache  map[string][]images.SubsonicSong
	albumCache map[string][]images.SubsonicAlbum
}

func resolveFromNavidrome(ctx context.Context, store *sqlite.Sqlite, apply, verbose bool, limit int) error {
	r := &navidromeResolver{
		client:     images.NewSubsonicClient(),
		songCache:  make(map[string][]images.SubsonicSong),
		albumCache: make(map[string][]images.SubsonicAlbum),
	}

	var kept, moved, unknown, merged int
	err := forEachPseudoTrack(store, func(p pseudoTrack) (bool, error) {
		if limit > 0 && kept+moved+unknown >= limit {
			return true, nil
		}

		album, legit, found, err := r.resolve(ctx, p)
		if err != nil {
			return false, err
		}

		// rule 1: a real album that happens to be named after the track
		if found && legit && strings.EqualFold(album, p.track) {
			kept++
			if verbose {
				count, err := r.songCount(ctx, album, p.artist)
				if err != nil {
					return false, err
				}
				fmt.Printf("keep   %-28s %-40s : legit album %q (%d tracks on server)\n",
					truncate(p.artist, 28), truncate(p.track, 40), truncate(album, 40), count)
			}
			return false, nil
		}

		target := album
		reason := "album on server"
		if !found {
			// rule 3: nothing known about the track, and no real album of that
			// name. An album named after the track with other tracks is legit.
			if legit {
				kept++
				if verbose {
					fmt.Printf("keep   %-28s %-40s : album %q exists with other tracks\n",
						truncate(p.artist, 28), truncate(p.track, 40), truncate(p.track, 40))
				}
				return false, nil
			}
			target = unknownAlbum
			reason = "album unknown"
		}

		action := "move  "
		targetID, isNew, err := r.releaseID(ctx, store, p.artistID, target, apply)
		if err != nil {
			return false, err
		}
		if !isNew {
			dupID, err := trackWithTitle(ctx, store, targetID, p.track)
			if err != nil {
				return false, err
			}
			if dupID != 0 && dupID != p.trackID {
				action = "merge "
				merged++
			}
		}
		if apply {
			if action == "merge " {
				dupID, err := trackWithTitle(ctx, store, targetID, p.track)
				if err != nil {
					return false, err
				}
				if err := store.MergeTracks(ctx, p.trackID, dupID); err != nil {
					return false, err
				}
			} else if err := store.MoveTrackToRelease(ctx, p.trackID, targetID); err != nil {
				return false, err
			}
		}
		if target == unknownAlbum {
			unknown++
		} else {
			moved++
		}
		fmt.Printf("%s %-28s %-40s : %q -> %q (%s)\n", action,
			truncate(p.artist, 28), truncate(p.track, 40),
			truncate(p.track, 40), truncate(target, 40), reason)
		return false, nil
	})

	fmt.Printf("\nmoved to the album known by the server: %d\n", moved)
	fmt.Printf("moved to %q:                        %d\n", unknownAlbum, unknown)
	fmt.Printf("merged into an existing track row:      %d\n", merged)
	fmt.Printf("kept as-is (real album):                %d\n", kept)
	return err
}

// resolve looks the track up on the server and returns the album it belongs to,
// whether that album is a real one (more than one track on the server) and
// whether the track was found at all.
func (r *navidromeResolver) resolve(ctx context.Context, p pseudoTrack) (album string, legit, found bool, err error) {
	songs, err := r.songs(ctx, p.track)
	if err != nil {
		return "", false, false, fmt.Errorf("resolve %q: %w", p.track, err)
	}
	for _, s := range songs {
		if strings.EqualFold(strings.TrimSpace(s.Title), p.track) &&
			strings.EqualFold(strings.TrimSpace(s.Artist), p.artist) && s.Album != "" {
			count, err := r.songCount(ctx, s.Album, p.artist)
			if err != nil {
				return "", false, false, fmt.Errorf("resolve %q: %w", p.track, err)
			}
			return s.Album, count > 1, true, nil
		}
	}

	// the track itself is unknown to the server; the album may still exist
	count, err := r.songCount(ctx, p.track, p.artist)
	if err != nil {
		return "", false, false, fmt.Errorf("resolve %q: %w", p.track, err)
	}
	if count > 1 {
		return p.track, true, false, nil
	}
	return "", false, false, nil
}

// songCount returns the number of tracks the server knows for an album of that
// name by the artist, or 0 when there is no such album.
func (r *navidromeResolver) songCount(ctx context.Context, album, artist string) (int, error) {
	albums, err := r.albums(ctx, album)
	if err != nil {
		return 0, err
	}
	for _, a := range albums {
		if strings.EqualFold(strings.TrimSpace(a.Name), album) &&
			(strings.EqualFold(strings.TrimSpace(a.Artist), artist) || a.Artist == "") {
			return a.SongCount, nil
		}
	}
	return 0, nil
}

func (r *navidromeResolver) songs(ctx context.Context, query string) ([]images.SubsonicSong, error) {
	if s, ok := r.songCache[query]; ok {
		return s, nil
	}
	s, err := r.client.SearchSongs(ctx, query)
	if err != nil {
		return nil, err
	}
	r.songCache[query] = s
	return s, nil
}

func (r *navidromeResolver) albums(ctx context.Context, query string) ([]images.SubsonicAlbum, error) {
	if a, ok := r.albumCache[query]; ok {
		return a, nil
	}
	a, err := r.client.SearchAlbums(ctx, query)
	if err != nil {
		return nil, err
	}
	r.albumCache[query] = a
	return a, nil
}

// releaseID returns the id of the release with the given title for the artist.
// An existing release is reused, except for "Unknown Album": that is
// deliberately never shared, so each unknown track gets its own release and
// unrelated songs are not presented as one album. When the release does not
// exist it is only created if create is true; isNew reports that it had to be
// created (or would have to be, in a dry run).
func (r *navidromeResolver) releaseID(ctx context.Context, store *sqlite.Sqlite, artistID int32, title string, create bool) (id int32, isNew bool, err error) {
	if title != unknownAlbum {
		rows, err := store.Query(`
			SELECT r.id FROM releases r
			JOIN release_aliases ra ON ra.release_id = r.id AND ra.is_primary = 1
			JOIN artist_releases ar ON ar.release_id = r.id
			WHERE ar.artist_id = ? AND lower(ra.alias) = lower(?)
			LIMIT 1`, artistID, title)
		if err != nil {
			return 0, false, fmt.Errorf("releaseID: %w", err)
		}
		if rows.Next() {
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, false, err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, false, err
		}
		if id != 0 {
			return id, false, nil
		}
	}
	if !create {
		return 0, true, nil
	}

	album, err := store.SaveAlbum(ctx, db.SaveAlbumOpts{
		Title:     title,
		ArtistIDs: []int32{artistID},
	})
	if err != nil {
		return 0, false, fmt.Errorf("releaseID: create %q: %w", title, err)
	}
	return album.ID, true, nil
}

// trackWithTitle returns the id of the track on the release whose title matches
// (case-insensitively), or 0 when there is none.
func trackWithTitle(ctx context.Context, store *sqlite.Sqlite, releaseID int32, title string) (int32, error) {
	rows, err := store.Query(`
		SELECT t.id FROM tracks t
		JOIN track_aliases ta ON ta.track_id = t.id AND ta.is_primary = 1
		WHERE t.release_id = ? AND lower(ta.alias) = lower(?)
		LIMIT 1`, releaseID, title)
	if err != nil {
		return 0, fmt.Errorf("trackWithTitle: %w", err)
	}
	defer rows.Close()
	var id int32
	if rows.Next() {
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
	}
	return id, rows.Err()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
