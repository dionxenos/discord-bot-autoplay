// Package youtube resolves search queries to playable YouTube results using yt-dlp.
package youtube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
