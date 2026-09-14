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

// killGracePeriod bounds how long playTrack waits for ffmpeg/yt-dlp to
// actually stop after being killed (via Skip/Stop) before giving up on the
// wait and moving on to the next track anyway. This exists because dca's
// own shutdown (its ffmpeg stderr-reader goroutine, specifically) can fail
// to notice a killed process promptly, which would otherwise stall the
// entire queue behind one skipped track.
const killGracePeriod = 3 * time.Second

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
	killSignal    chan struct{}
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
	cmd := p.ytdlp
	cur := p.current
	killSig := p.killSignal
	p.mu.Unlock()
	if enc == nil {
		return false
	}
	title := "<unknown>"
	if cur != nil {
		title = cur.Title
	}
	log.Printf("[guild %s] Skip() invoked, skipping %q", p.guildID, title)
	enc.Stop()
	killProcess(cmd)
	signalKilled(killSig)
	return true
}

// Stop clears the queue, stops playback, and leaves the voice channel.
func (p *GuildPlayer) Stop() {
	p.mu.Lock()
	p.queue = nil
	enc := p.encode
	cmd := p.ytdlp
	killSig := p.killSignal
	running := p.running
	p.mu.Unlock()

	if enc != nil {
		enc.Stop()
	}
	killProcess(cmd)
	signalKilled(killSig)
	if running {
		select {
		case p.quitCh <- struct{}{}:
		default:
		}
	}
}

// killProcess kills cmd if it's still running. enc, cmd, and killSig must
// all be captured together under a single lock acquisition by the caller:
// reading them separately (multiple lock/unlock cycles) leaves a window
// where the currently playing track can finish and the run loop can advance
// to the next queued track — setting p.ytdlp etc. to the *new* track's
// state — before a later read happens, causing this to act on the wrong
// (freshly started) track.
func killProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		log.Printf("killing yt-dlp pid=%d", cmd.Process.Pid)
		_ = cmd.Process.Kill()
	}
}

// signalKilled notifies playTrack (via its per-track killSignal channel)
// that a kill was just requested, so it can bound how long it waits for
// dca/ffmpeg to actually finish shutting down instead of waiting forever.
func signalKilled(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
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
			remaining := len(p.queue)
			p.mu.Unlock()

			log.Printf("[guild %s] dequeued %q, %d remaining in queue", p.guildID, track.Title, remaining)

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
	log.Printf("[guild %s] playTrack starting %q", p.guildID, track.Title)

	killSignal := make(chan struct{}, 1)

	p.mu.Lock()
	p.current = track
	p.killSignal = killSignal
	p.mu.Unlock()

	defer func() {
		log.Printf("[guild %s] playTrack returning for %q", p.guildID, track.Title)
		p.mu.Lock()
		p.current = nil
		p.encode = nil
		p.stream = nil
		p.ytdlp = nil
		p.killSignal = nil
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
	log.Printf("[guild %s] started yt-dlp pid=%d for %q", p.guildID, ytdlpCmd.Process.Pid, track.Title)

	p.mu.Lock()
	p.ytdlp = ytdlpCmd
	p.mu.Unlock()

	defer func() {
		// Run in the background: never block playTrack's return on reaping
		// this process. See the killGracePeriod comment for why a killed
		// child can, in rare cases, take a while to be fully waited on.
		go func() {
			if ytdlpCmd.Process != nil {
				_ = ytdlpCmd.Process.Kill()
			}
			_ = ytdlpCmd.Wait()
		}()
	}()

	opts := *dca.StdEncodeOptions
	opts.Bitrate = 96
	opts.Application = dca.AudioApplicationAudio

	encode, err := dca.EncodeMem(stdout, &opts)
	if err != nil {
		p.notify("❌ Couldn't play **" + track.Title + "**: " + err.Error())
		return
	}
	defer func() {
		// Run in the background: dca's own shutdown can hang after a killed
		// ffmpeg process on some platforms (see killGracePeriod), and
		// playTrack must never block here or the whole queue stalls behind
		// it. Worst case this leaks one goroutine per such occurrence.
		go encode.Cleanup()
	}()

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

	select {
	case err = <-done:
	case <-killSignal:
		log.Printf("[guild %s] kill signal received for %q, waiting up to %s for stream to stop", p.guildID, track.Title, killGracePeriod)
		select {
		case err = <-done:
		case <-time.After(killGracePeriod):
			log.Printf("[guild %s] %q did not stop within %s of being killed; abandoning wait", p.guildID, track.Title, killGracePeriod)
		}
	}

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
