package main

import (
	"log"
	"os"

	"github.com/slack-go/slack"
)

func main() {
	cfg := LoadConfig()
	log.Printf(
		"Config loaded. Team=%s Managers=%d TeamMembers=%d Timezone=%s LLMBatchSize=%d LLMConfidenceThreshold=%.2f LLMExampleCount=%d LLMExampleMaxChars=%d LLMGlossaryPath=%s",
		cfg.TeamName,
		len(cfg.ManagerSlackIDs),
		len(cfg.TeamMembers),
		cfg.Timezone,
		cfg.LLMBatchSize,
		cfg.LLMConfidence,
		cfg.LLMExampleCount,
		cfg.LLMExampleMaxLen,
		cfg.LLMGlossaryPath,
	)

	db, err := InitDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("Failed to init database: %v", err)
	}
	log.Printf("Database initialized at %s", cfg.DBPath)
	defer db.Close()

	os.MkdirAll(cfg.ReportOutputDir, 0755)
	log.Printf("Report output dir: %s", cfg.ReportOutputDir)

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
