// Package player manages per-guild voice connections and playback queues.
package player

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jonas747/dca"
)

// idleTimeout is how long the bot waits in an empty queue before leaving
// the voice channel.
const idleTimeout = 3 * time.Minute

// minPlaybackForSuccess is the playback duration below which a finished
// track is treated as a likely failure (bad URL, network error, etc.) worth
// surfacing diagnostics for, rather than a normal end-of-song.
const minPlaybackForSuccess = 2 * time.Second

// Track is a single queued/playing song.
type Track struct {
	Title       string
	WebpageURL  string
	Duration    time.Duration
	RequestedBy string
}

// GuildPlayer owns the voice connection and queue for a single guild.
type GuildPlayer struct {
	session *discordgo.Session
	guildID string

	mu            sync.Mutex
	vc            *discordgo.VoiceConnection
	textChannelID string
	queue         []*Track
	current       *Track
	ytdlp         *exec.Cmd
	encode        *dca.EncodeSession
	stream        *dca.StreamingSession
	running       bool

	wakeCh chan struct{}
	quitCh chan struct{}
}

func newGuildPlayer(s *discordgo.Session, guildID string) *GuildPlayer {
	return &GuildPlayer{
		session: s,
		guildID: guildID,
		wakeCh:  make(chan struct{}, 1),
		quitCh:  make(chan struct{}, 1),
	}
}

// Enqueue adds a track to the guild's queue, starting the playback loop and
// joining voiceChannelID if it isn't already running. It reports the track's
// position in the queue (1 means it will play next/immediately) and whether
// the loop had to be started.
func (p *GuildPlayer) Enqueue(voiceChannelID, textChannelID string, track *Track) (position int, startingNow bool) {
	p.mu.Lock()
	p.textChannelID = textChannelID
	p.queue = append(p.queue, track)
	position = len(p.queue)
	running := p.running
	if !running {
		p.running = true
	}
	p.mu.Unlock()

	if !running {
		startingNow = true
		go p.run(voiceChannelID)
	} else {
		select {
		case p.wakeCh <- struct{}{}:
		default:
		}
	}
	return position, startingNow
}

// Skip stops the currently playing track, if any, causing the loop to
// advance to the next queued track. Returns false if nothing was playing.
func (p *GuildPlayer) Skip() bool {
	p.mu.Lock()
	enc := p.encode
	p.mu.Unlock()
	if enc == nil {
		return false
	}
	enc.Stop()
	p.killYtdlp()
	return true
}

// Stop clears the queue, stops playback, and leaves the voice channel.
func (p *GuildPlayer) Stop() {
	p.mu.Lock()
	p.queue = nil
	enc := p.encode
	running := p.running
	p.mu.Unlock()

	if enc != nil {
		enc.Stop()
	}
	p.killYtdlp()
	if running {
		select {
		case p.quitCh <- struct{}{}:
		default:
		}
	}
}

func (p *GuildPlayer) killYtdlp() {
	p.mu.Lock()
	cmd := p.ytdlp
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// Pause pauses the current stream. Returns false if nothing was playing.
func (p *GuildPlayer) Pause() bool {
	p.mu.Lock()
	s := p.stream
	p.mu.Unlock()
	if s == nil {
		return false
	}
	s.SetPaused(true)
	return true
}

// Resume unpauses the current stream. Returns false if nothing was playing.
func (p *GuildPlayer) Resume() bool {
	p.mu.Lock()
	s := p.stream
	p.mu.Unlock()
	if s == nil {
		return false
	}
	s.SetPaused(false)
	return true
}

// Status returns the currently playing track (nil if none) and a snapshot
// of the upcoming queue.
func (p *GuildPlayer) Status() (current *Track, queue []*Track) {
	p.mu.Lock()
	defer p.mu.Unlock()
	queue = make([]*Track, len(p.queue))
	copy(queue, p.queue)
	return p.current, queue
}

func (p *GuildPlayer) run(voiceChannelID string) {
	vc, err := p.session.ChannelVoiceJoin(p.guildID, voiceChannelID, false, false)
	if err != nil {
		p.notify("❌ Failed to join voice channel: " + err.Error())
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
		return
	}
	vc.LogLevel = discordgo.LogWarning

	p.mu.Lock()
	p.vc = vc
	p.mu.Unlock()

	idleTimer := time.NewTimer(idleTimeout)
	defer idleTimer.Stop()

	for {
		p.mu.Lock()
		if len(p.queue) > 0 {
			track := p.queue[0]
			p.queue = p.queue[1:]
			p.mu.Unlock()

			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}

			p.playTrack(vc, track)

			idleTimer.Reset(idleTimeout)
			continue
		}
		p.mu.Unlock()

		select {
		case <-p.wakeCh:
			continue
		case <-p.quitCh:
			p.disconnect(vc)
			return
		case <-idleTimer.C:
			p.notify("👋 Leaving voice channel due to inactivity.")
			p.disconnect(vc)
			return
		}
	}
}

func (p *GuildPlayer) disconnect(vc *discordgo.VoiceConnection) {
	_ = vc.Disconnect()
	p.mu.Lock()
	p.vc = nil
	p.running = false
	p.mu.Unlock()
}

func (p *GuildPlayer) playTrack(vc *discordgo.VoiceConnection, track *Track) {
	p.mu.Lock()
	p.current = track
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.current = nil
		p.encode = nil
		p.stream = nil
		p.ytdlp = nil
		p.mu.Unlock()
	}()

	p.notify(fmt.Sprintf("🎶 Now playing: **%s** `[%s]` (requested by %s)", track.Title, formatDuration(track.Duration), track.RequestedBy))

	// Stream the audio through yt-dlp itself rather than handing ffmpeg a
	// resolved CDN URL directly: YouTube's CDN frequently rejects direct
	// fetches from a process other than yt-dlp (missing headers/throttling
	// tokens), and a resolved URL can expire while a track waits in queue.
	ytdlpCmd := exec.Command("yt-dlp",
		"--no-playlist",
		"--no-warnings",
		"-f", "bestaudio/best",
		"-o", "-",
		track.WebpageURL,
	)

	stdout, err := ytdlpCmd.StdoutPipe()
	if err != nil {
		p.notify("❌ Couldn't prepare audio for **" + track.Title + "**: " + err.Error())
		return
	}
	var stderrBuf bytes.Buffer
	ytdlpCmd.Stderr = &stderrBuf

	if err := ytdlpCmd.Start(); err != nil {
		p.notify("❌ Couldn't start yt-dlp for **" + track.Title + "**: " + err.Error())
		return
	}

	p.mu.Lock()
	p.ytdlp = ytdlpCmd
	p.mu.Unlock()

	defer func() {
		if ytdlpCmd.Process != nil {
			_ = ytdlpCmd.Process.Kill()
		}
		_ = ytdlpCmd.Wait()
	}()

	opts := *dca.StdEncodeOptions
	opts.Bitrate = 96
	opts.Application = dca.AudioApplicationAudio

	encode, err := dca.EncodeMem(stdout, &opts)
	if err != nil {
		p.notify("❌ Couldn't play **" + track.Title + "**: " + err.Error())
		return
	}
	defer encode.Cleanup()

	p.mu.Lock()
	p.encode = encode
	p.mu.Unlock()

	_ = vc.Speaking(true)
	defer vc.Speaking(false)

	done := make(chan error, 1)
	stream := dca.NewStream(encode, vc, done)

	p.mu.Lock()
	p.stream = stream
	p.mu.Unlock()

	err = <-done
	if err != nil && err != io.EOF {
		log.Printf("[guild %s] playback error for %q: %v", p.guildID, track.Title, err)
	}

	if stream.PlaybackPosition() < minPlaybackForSuccess {
		if stderrBuf.Len() > 0 {
			log.Printf("[guild %s] yt-dlp stderr for %q:\n%s", p.guildID, track.Title, strings.TrimSpace(stderrBuf.String()))
		}
		if msgs := strings.TrimSpace(encode.FFMPEGMessages()); msgs != "" {
			log.Printf("[guild %s] ffmpeg messages for %q:\n%s", p.guildID, track.Title, msgs)
		}
		p.notify("⚠️ Playback of **" + track.Title + "** stopped almost immediately — check the bot console for the actual error.")
	}
}

func (p *GuildPlayer) notify(msg string) {
	p.mu.Lock()
	ch := p.textChannelID
	p.mu.Unlock()
	if ch == "" {
		return
	}
	if _, err := p.session.ChannelMessageSend(ch, msg); err != nil {
		log.Printf("failed to send message to channel %s: %v", ch, err)
	}
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "live"
	}
	total := int(d.Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// FormatDuration exposes the duration formatting helper for use by callers
// that render track info outside of playTrack (e.g. the queue listing).
func FormatDuration(d time.Duration) string {
	return formatDuration(d)
}
