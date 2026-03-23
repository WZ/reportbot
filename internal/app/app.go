package app

import (
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"reportbot/internal/config"
	"reportbot/internal/fetch"
	"reportbot/internal/httpx"
	slackbot "reportbot/internal/integrations/slack"
	"reportbot/internal/nudge"
	"reportbot/internal/storage/sqlite"
	"reportbot/internal/web/handlers"

	"github.com/slack-go/slack"
)

func Main() {
	cfg := config.LoadConfig()
	appliedHTTPTimeout := httpx.ConfigureExternalHTTPClient(cfg.ExternalHTTPTimeoutSeconds, cfg.TLSSkipVerify)
	log.Printf(
		"Config loaded. Team=%s Managers=%d TeamMembers=%d Timezone=%s LLMBatchSize=%d LLMConfidenceThreshold=%.2f LLMExampleCount=%d LLMExampleMaxChars=%d LLMGlossaryPath=%s OpenAIBaseURL=%s ExternalHTTPTimeout=%s",
		cfg.TeamName,
		len(cfg.ManagerSlackIDs),
		len(cfg.TeamMembers),
		cfg.Timezone,
		cfg.LLMBatchSize,
		cfg.LLMConfidence,
		cfg.LLMExampleCount,
		cfg.LLMExampleMaxLen,
		cfg.LLMGlossaryPath,
		cfg.OpenAIBaseURL,
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

	nudge.StartNudgeScheduler(cfg, db, api)
	fetch.StartAutoFetchScheduler(cfg, db, api)

	// Start web UI if enabled
	var webSrv *http.Server
	if cfg.WebEnabled {
		webSrv = handlers.NewServer(cfg, db)
		go func() {
			log.Printf("Web UI listening on :%d", cfg.WebPort)
			if err := webSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("Web server error: %v", err)
			}
		}()
	}

	// App-level signal handler for graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		log.Println("Shutting down...")
		if webSrv != nil {
			webSrv.Close()
		}
		os.Exit(0)
	}()

	log.Println("Starting Engineering Report Bot...")
	if err := slackbot.StartSlackBot(cfg, db, api); err != nil {
		log.Fatalf("Slack bot error: %v", err)
	}
}
