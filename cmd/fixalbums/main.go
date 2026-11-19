// Command fixalbums repairs releases that hold a track whose title was used as
// its own release title. The Last.fm importer substitutes the track title when
// an export entry has no album:
//
//	album := track.Album.Text
//	if album == "" { album = track.Name }
//
// which leaves pseudo-albums behind: a release "Falling" containing the single
// track "Falling". For every such pseudo-album the tool looks for another track
// of the same artist + title that sits on a plausible album and merges the two
// tracks, so that all listens move to the real album and the pseudo-release is
// removed. Ambiguous and unresolvable cases are left untouched.
//
// The tool never fetches metadata and is safe to re-run. By default it only
// reports what it would do; pass -apply to write.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/gabehf/koito/internal/cfg"
	"github.com/gabehf/koito/internal/db/sqlite"
	"github.com/gabehf/koito/internal/logger"
)

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
	verbose := flag.Bool("v", false, "print skipped cases with the reason")
	limit := flag.Int("limit", 0, "process at most N pseudo tracks (0 = no limit)")
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

	moved, skipped, err := fixTracks(ctx, store, *apply, *verbose, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixalbums: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\ntracks merged into a real album: %d\n", moved)
	fmt.Printf("tracks left untouched:           %d\n", skipped)
	if !*apply {
		fmt.Println("\ndry run: nothing was written. re-run with -apply to apply these changes")
	}
}

func fixTracks(ctx context.Context, store *sqlite.Sqlite, apply, verbose bool, limit int) (int, int, error) {
	moved, skipped := 0, 0
	lastID := int32(0)
	// a track with several artists shows up once per artist; handle it only once
	handled := make(map[int32]bool)

	for {
		rows, err := store.Query(pseudoTrackQuery, lastID)
		if err != nil {
			return moved, skipped, fmt.Errorf("query pseudo tracks: %w", err)
		}
		var batch []pseudoTrack
		for rows.Next() {
			var p pseudoTrack
			if err := rows.Scan(&p.trackID, &p.track, &p.artistID, &p.artist, &p.releaseID); err != nil {
				rows.Close()
				return moved, skipped, err
			}
			batch = append(batch, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return moved, skipped, err
		}
		if len(batch) == 0 {
			return moved, skipped, nil
		}

		for _, p := range batch {
			lastID = p.trackID
			if handled[p.trackID] {
				continue
			}
			handled[p.trackID] = true
			if limit > 0 && moved+skipped >= limit {
				return moved, skipped, nil
			}

			cands, err := candidatesFor(store, p)
			if err != nil {
				return moved, skipped, err
			}
			target, ok, reason := choose(cands)
			if !ok {
				skipped++
				if verbose {
					fmt.Printf("keep   %-28s %-40s : %s\n", truncate(p.artist, 28), truncate(p.track, 40), reason)
				}
				continue
			}
			fmt.Printf("merge  %-28s %-40s : %q -> %q\n", truncate(p.artist, 28), truncate(p.track, 40), truncate(p.track, 40), truncate(target.title, 40))
			if apply {
				if err := store.MergeTracks(ctx, p.trackID, target.trackID); err != nil {
					return moved, skipped, err
				}
			}
			moved++
		}
	}
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

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
