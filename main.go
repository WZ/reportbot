package main

import (
	"log"
	"os"

	"github.com/slack-go/slack"
)

func main() {
	cfg := LoadConfig()

	db, err := InitDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("Failed to init database: %v", err)
	}
	defer db.Close()

	os.MkdirAll(cfg.ReportOutputDir, 0755)

	api := slack.New(
		cfg.SlackBotToken,
		slack.OptionAppLevelToken(cfg.SlackAppToken),
	)

	StartNudgeScheduler(cfg, api)

	log.Println("Starting Engineering Report Bot...")
	if err := StartSlackBot(cfg, db, api); err != nil {
		log.Fatalf("Slack bot error: %v", err)
	}
}
