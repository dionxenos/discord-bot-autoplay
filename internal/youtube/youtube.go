// Package youtube resolves search queries to playable YouTube results using yt-dlp.
package youtube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// fieldSep separates the fields yt-dlp prints for a result. It's chosen to be
// extremely unlikely to appear in a video title.
const fieldSep = "\x1f"

// Track is a single YouTube search result.
type Track struct {
	Title      string
	WebpageURL string
	Duration   time.Duration
}

// SearchFirst searches YouTube for query and returns the first result's
// metadata. It deliberately does not resolve a direct media URL: those are
// short-lived and often rejected by YouTube's CDN when fetched outside of
// yt-dlp's own request context, so playback instead re-invokes yt-dlp on
// WebpageURL at stream time (see player.playTrack).
func SearchFirst(ctx context.Context, query string) (*Track, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("empty search query")
	}

	args := []string{
		"--no-playlist",
		"--skip-download",
		"--no-warnings",
		"--default-search", "ytsearch",
		"--print", strings.Join([]string{"%(title)s", "%(webpage_url)s", "%(duration)s"}, fieldSep),
		fmt.Sprintf("ytsearch1:%s", query),
	}

	cmd := exec.CommandContext(ctx, "yt-dlp", args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("yt-dlp: %s", msg)
	}

	line := strings.TrimSpace(stdout.String())
	if line == "" {
		return nil, errors.New("no results found")
	}
	// yt-dlp can print one line per remaining warning followed by the actual
	// result; the result is always the last non-empty line.
	lines := strings.Split(line, "\n")
	line = strings.TrimSpace(lines[len(lines)-1])

	parts := strings.SplitN(line, fieldSep, 3)
	if len(parts) < 3 {
		return nil, fmt.Errorf("unexpected yt-dlp output: %q", line)
	}

	title, webpageURL, durationStr := parts[0], parts[1], parts[2]

	var duration time.Duration
	if secs, err := strconv.ParseFloat(durationStr, 64); err == nil {
		duration = time.Duration(secs * float64(time.Second))
	}

	return &Track{
		Title:      title,
		WebpageURL: webpageURL,
		Duration:   duration,
	}, nil
}

// SearchRelated returns up to count tracks related to seedURL, drawn from
// YouTube's "Mix" radio playlist for that video (the same recommendation
// mechanism YouTube's autoplay uses). The seed video itself is excluded from
// the results.
func SearchRelated(ctx context.Context, seedURL string, count int) ([]*Track, error) {
	if count <= 0 {
		return nil, nil
	}

	videoID := extractVideoID(seedURL)
	if videoID == "" {
		return nil, errors.New("could not determine video ID for related search")
	}

	mixURL := fmt.Sprintf("https://www.youtube.com/watch?v=%s&list=RD%s", videoID, videoID)

	args := []string{
		"--no-warnings",
		"--flat-playlist",
		"--playlist-end", strconv.Itoa(count + 1),
		"--print", strings.Join([]string{"%(id)s", "%(title)s", "%(duration)s"}, fieldSep),
		mixURL,
	}

	cmd := exec.CommandContext(ctx, "yt-dlp", args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("yt-dlp: %s", msg)
	}

	var tracks []*Track
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, fieldSep, 3)
		if len(parts) < 3 {
			continue
		}

		id, title, durationStr := parts[0], parts[1], parts[2]
		if id == videoID {
			// The seed video is usually included as the first playlist entry.
			continue
		}

		var duration time.Duration
		if secs, err := strconv.ParseFloat(durationStr, 64); err == nil {
			duration = time.Duration(secs * float64(time.Second))
		}

		tracks = append(tracks, &Track{
			Title:      title,
			WebpageURL: "https://www.youtube.com/watch?v=" + id,
			Duration:   duration,
		})
		if len(tracks) >= count {
			break
		}
	}

	if len(tracks) == 0 {
		return nil, errors.New("no related tracks found")
	}
	return tracks, nil
}

// extractVideoID pulls the 11-character video ID out of a YouTube watch or
// short URL. Returns "" if none is found.
func extractVideoID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	if id := u.Query().Get("v"); id != "" {
		return id
	}
	if strings.Contains(u.Host, "youtu.be") {
		return strings.Trim(u.Path, "/")
	}
	return ""
}
