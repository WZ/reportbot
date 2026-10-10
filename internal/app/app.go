package app

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/cutover"
	"reportbot/internal/fetch"
	"reportbot/internal/filing"
	"reportbot/internal/httpx"
	"reportbot/internal/integrations/decision"
	"reportbot/internal/integrations/github"
	"reportbot/internal/integrations/gitlab"
	"reportbot/internal/integrations/llm"
	slackbot "reportbot/internal/integrations/slack"
	"reportbot/internal/nudge"
	"reportbot/internal/storage/sqlite"
	"reportbot/internal/web/handlers"

	"github.com/slack-go/slack"
)

func Main() {
	if len(os.Args) > 1 && os.Args[1] == "migrate-v2" {
		migrateV2(os.Args[2:])
		return
	}

	cfg := config.LoadConfig()
	appliedHTTPTimeout := httpx.ConfigureExternalHTTPClient(cfg.ExternalHTTPTimeoutSeconds, cfg.TLSSkipVerify)
	log.Printf(
		"Config loaded. Team=%s Managers=%d TeamMembers=%d Timezone=%s LLMProvider=%s LLMModel=%s LLMBatchSize=%d LLMExampleCount=%d DecisionModel=%s DecisionModelConfidence=%.2f ExternalHTTPTimeout=%s",
		cfg.TeamName, len(cfg.ManagerSlackIDs), len(cfg.TeamMembers), cfg.Timezone, cfg.LLMProvider, llm.ModelName(cfg),
		cfg.LLMBatchSize, cfg.LLMExampleCount, cfg.DecisionModelName, cfg.DecisionModelConfidence, appliedHTTPTimeout,
	)

	db, err := sqlite.InitDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("Failed to init database: %v", err)
	}
	log.Printf("Database initialized at %s", cfg.DBPath)
	defer db.Close()

	if err := os.MkdirAll(cfg.ReportOutputDir, 0755); err != nil {
		log.Fatalf("Failed to create report output directory %s: %v", cfg.ReportOutputDir, err)
	}
	log.Printf("Report output dir: %s", cfg.ReportOutputDir)

	api := slack.New(
		cfg.SlackBotToken,
		slack.OptionAppLevelToken(cfg.SlackAppToken),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := filing.NewQueue(&filing.Filer{
		DB:       db,
		Cfg:      cfg,
		Classify: llm.ClassifySections,
		Decision: decision.NewClient(cfg.DecisionModelURL, cfg.DecisionModelAPIKey, cfg.DecisionModelName),
		Notify:   slackbot.NewNotifier(api, db, cfg),
	})
	queue.Start(ctx)

	nudge.StartNudgeScheduler(cfg, db, api)
	fetch.StartAutoFetchScheduler(cfg, db, api, fetch.Hooks{ResolveAccount: cfg.AccountAuthorID, Wake: queue.Wake})

	// Start web UI if enabled — verify bind before proceeding
	var webSrv *http.Server
	if cfg.WebEnabled {
		webSrv = handlers.NewServer(cfg, db, queue.Wake)
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.WebPort))
		if err != nil {
			log.Fatalf("Web server bind failed on port %d: %v", cfg.WebPort, err)
		}
		log.Printf("Web UI listening on :%d", cfg.WebPort)
		go func() {
			if err := webSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
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
		cancel()
		if webSrv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := webSrv.Shutdown(ctx); err != nil {
				log.Printf("Web server shutdown error: %v", err)
			}
		}
		db.Close()
		os.Exit(0)
	}()

	log.Println("Starting Engineering Report Bot...")
	if err := slackbot.StartSlackBot(cfg, db, api, queue.Wake); err != nil {
		log.Fatalf("Slack bot error: %v", err)
	}
}

// migrateV2 is the one-off cut-over: seed the Taxonomy and Work Items from published
// reports, and (with --drop-v1, after a backup) drop V1 tables and columns.
func migrateV2(args []string) {
	fs := flag.NewFlagSet("migrate-v2", flag.ExitOnError)
	latest := fs.String("report", "", "path to the latest published team report (NAME_YYYYMMDD.md)")
	history := fs.String("history", "", "folder of published reports used to promote existing rows (default: the --report folder)")
	dropV1 := fs.Bool("drop-v1", false, "after the import, back up the database and drop V1 tables and columns")
	dryRun := fs.Bool("dry-run", false, "parse and count only; write nothing")
	suggest := fs.Bool("suggest-author-ids", false, "print a suggested author_ids block from roster names; write nothing")
	_ = fs.Parse(args)
	if *latest == "" {
		fs.Usage()
		os.Exit(2)
	}

	cfg := config.LoadConfig()
	httpx.ConfigureExternalHTTPClient(cfg.ExternalHTTPTimeoutSeconds, cfg.TLSSkipVerify)
	if *suggest {
		authors, err := cutover.ReportAuthors(*latest)
		if err != nil {
			log.Fatalf("migrate-v2: %v", err)
		}
		accounts, err := recentAccounts(cfg)
		if err != nil {
			log.Fatalf("migrate-v2: no suggestions: %v", err)
		}
		out, err := slackbot.SuggestAuthorIDs(cfg, authors, accounts, slack.New(cfg.SlackBotToken).GetUserInfo)
		if err != nil {
			log.Fatalf("migrate-v2: no suggestions: %v", err)
		}
		fmt.Print(out)
		return
	}
	db, err := sqlite.InitDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("Failed to init database: %v", err)
	}
	defer db.Close()

	sum, err := cutover.Run(db, cfg.DBPath, cutover.Options{
		LatestReport: *latest,
		HistoryDir:   *history,
		DropV1:       *dropV1,
		DryRun:       *dryRun,
		AuthorID:     cfg.AuthorID,
		Location:     cfg.Location,
	})
	fmt.Print(sum.String())
	if err != nil {
		log.Fatalf("migrate-v2: %v", err)
	}
}

// recentAccounts lists the authors of the last four weeks' MRs/PRs for author_ids suggestions.
func recentAccounts(cfg config.Config) ([]slackbot.Account, error) {
	to := time.Now().In(cfg.Location)
	from := to.AddDate(0, 0, -28)
	var out []slackbot.Account
	if cfg.GitLabConfigured() {
		mrs, err := gitlab.FetchMRs(cfg, from, to)
		if err != nil {
			return nil, fmt.Errorf("fetching GitLab MRs: %w", err)
		}
		for _, mr := range mrs {
			out = append(out, slackbot.Account{Host: "gitlab", ID: mr.AuthorID, Username: mr.Author, Name: mr.AuthorName})
		}
	}
	if cfg.GitHubConfigured() {
		prs, err := github.FetchGitHubPRs(cfg, from, to)
		if err != nil {
			return nil, fmt.Errorf("fetching GitHub PRs: %w", err)
		}
		for _, pr := range prs {
			out = append(out, slackbot.Account{Host: "github", ID: pr.AuthorID, Username: pr.Author, Name: pr.AuthorName})
		}
	}
	return out, nil
}
