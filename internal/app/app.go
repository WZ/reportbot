package app

import (
	"log"
	"os"
	"reportbot/internal/config"
	"reportbot/internal/fetch"
	"reportbot/internal/httpx"
	slackbot "reportbot/internal/integrations/slack"
	"reportbot/internal/nudge"
	"reportbot/internal/storage/sqlite"

	"github.com/slack-go/slack"
)

func Main() {
	cfg := config.LoadConfig()
	appliedHTTPTimeout := httpx.ConfigureExternalHTTPClient(cfg.ExternalHTTPTimeoutSeconds)
	log.Printf(
		"Config loaded. Team=%s Managers=%d TeamMembers=%d Timezone=%s LLMBatchSize=%d LLMConfidenceThreshold=%.2f LLMExampleCount=%d LLMExampleMaxChars=%d LLMGlossaryPath=%s ExternalHTTPTimeout=%s",
		cfg.TeamName,
		len(cfg.ManagerSlackIDs),
		len(cfg.TeamMembers),
		cfg.Timezone,
		cfg.LLMBatchSize,
		cfg.LLMConfidence,
		cfg.LLMExampleCount,
		cfg.LLMExampleMaxLen,
		cfg.LLMGlossaryPath,
		appliedHTTPTimeout,
	)

	db, err := sqlite.InitDB(cfg.DBPath)
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

	nudge.StartNudgeScheduler(cfg, api)
	fetch.StartAutoFetchScheduler(cfg, db, api)

	log.Println("Starting Engineering Report Bot...")
	if err := slackbot.StartSlackBot(cfg, db, api); err != nil {
		log.Fatalf("Slack bot error: %v", err)
	}
}
