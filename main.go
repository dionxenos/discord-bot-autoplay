package main

import (
	"bufio"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bwmarrin/discordgo"

	"musicbot/internal/bot"
)

func main() {
	loadDotEnv(".env")

	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_BOT_TOKEN environment variable is not set (see .env.example)")
	}

	prefix := os.Getenv("BOT_PREFIX")
	if prefix == "" {
		prefix = "!"
	}

	session, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("failed to create discord session: %v", err)
	}
	session.LogLevel = discordgo.LogWarning

	session.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsGuildVoiceStates |
		discordgo.IntentsMessageContent

	b := bot.New(session, prefix)
	b.RegisterHandlers()

	if err := session.Open(); err != nil {
		log.Fatalf("failed to open discord session: %v", err)
	}
	defer session.Close()

	log.Printf("Bot is running (prefix %q). Press Ctrl+C to exit.", prefix)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down...")
	b.Shutdown()
}

// loadDotEnv loads simple KEY=VALUE lines from path into the environment,
// without overriding variables that are already set. Missing files are
// silently ignored.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value)
		}
	}
}
