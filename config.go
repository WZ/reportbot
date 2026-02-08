package main

import (
	"log"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

var defaultCategories = []string{
	"Backend",
	"Frontend",
	"Infrastructure",
	"Bug Fixes",
	"Documentation",
}

type Config struct {
	SlackBotToken string `yaml:"slack_bot_token"`
	SlackAppToken string `yaml:"slack_app_token"`

	GitLabURL     string `yaml:"gitlab_url"`
	GitLabToken   string `yaml:"gitlab_token"`
	GitLabGroupID string `yaml:"gitlab_group_id"`

	LLMProvider     string `yaml:"llm_provider"`
	LLMModel        string `yaml:"llm_model"`
	AnthropicAPIKey string `yaml:"anthropic_api_key"`
	OpenAIAPIKey    string `yaml:"openai_api_key"`

	DBPath          string `yaml:"db_path"`
	ReportOutputDir string `yaml:"report_output_dir"`

	ManagerSlackIDs []string `yaml:"manager_slack_ids"`
	TeamMembers     []string `yaml:"team_members"`
	NudgeDay        string   `yaml:"nudge_day"`
	NudgeTime       string   `yaml:"nudge_time"`
	Categories      []string `yaml:"categories"`
	TeamName        string   `yaml:"team_name"`
}

func LoadConfig() Config {
	var cfg Config

	// Load from config.yaml if it exists
	configPath := "config.yaml"
	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}
	if data, err := os.ReadFile(configPath); err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			log.Fatalf("Error parsing %s: %v", configPath, err)
		}
		log.Printf("Loaded config from %s", configPath)
	}

	// Env vars override YAML values
	envOverride(&cfg.SlackBotToken, "SLACK_BOT_TOKEN")
	envOverride(&cfg.SlackAppToken, "SLACK_APP_TOKEN")
	envOverride(&cfg.GitLabURL, "GITLAB_URL")
	envOverride(&cfg.GitLabToken, "GITLAB_TOKEN")
	envOverride(&cfg.GitLabGroupID, "GITLAB_GROUP_ID")
	envOverride(&cfg.LLMProvider, "LLM_PROVIDER")
	envOverride(&cfg.LLMModel, "LLM_MODEL")
	envOverride(&cfg.AnthropicAPIKey, "ANTHROPIC_API_KEY")
	envOverride(&cfg.OpenAIAPIKey, "OPENAI_API_KEY")
	envOverride(&cfg.DBPath, "DB_PATH")
	envOverride(&cfg.ReportOutputDir, "REPORT_OUTPUT_DIR")
	envOverride(&cfg.TeamName, "TEAM_NAME")
	envOverride(&cfg.NudgeDay, "NUDGE_DAY")
	envOverride(&cfg.NudgeTime, "NUDGE_TIME")

	if ids := os.Getenv("MANAGER_SLACK_IDS"); ids != "" {
		cfg.ManagerSlackIDs = nil
		for _, id := range strings.Split(ids, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				cfg.ManagerSlackIDs = append(cfg.ManagerSlackIDs, id)
			}
		}
	}

	// Defaults
	if cfg.LLMProvider == "" {
		cfg.LLMProvider = "anthropic"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "./reportbot.db"
	}
	if cfg.ReportOutputDir == "" {
		cfg.ReportOutputDir = "./reports"
	}
	if cfg.NudgeDay == "" {
		cfg.NudgeDay = "Friday"
	}
	if cfg.NudgeTime == "" {
		cfg.NudgeTime = "10:00"
	}
	if cfg.TeamName == "" {
		cfg.TeamName = "My Team"
	}
	if len(cfg.Categories) == 0 {
		cfg.Categories = defaultCategories
	}

	// Validate required fields
	required := map[string]string{
		"slack_bot_token": cfg.SlackBotToken,
		"slack_app_token": cfg.SlackAppToken,
		"gitlab_url":      cfg.GitLabURL,
		"gitlab_token":    cfg.GitLabToken,
		"gitlab_group_id": cfg.GitLabGroupID,
	}
	for name, val := range required {
		if val == "" {
			log.Fatalf("Required config '%s' is not set (via config.yaml or env var)", name)
		}
	}

	switch cfg.LLMProvider {
	case "anthropic":
		if cfg.AnthropicAPIKey == "" {
			log.Fatalf("anthropic_api_key is required when llm_provider=anthropic")
		}
	case "openai":
		if cfg.OpenAIAPIKey == "" {
			log.Fatalf("openai_api_key is required when llm_provider=openai")
		}
	default:
		log.Fatalf("llm_provider must be 'anthropic' or 'openai', got '%s'", cfg.LLMProvider)
	}

	return cfg
}

func envOverride(field *string, envKey string) {
	if val := os.Getenv(envKey); val != "" {
		*field = val
	}
}

func (c Config) IsManager(slackUserID string) bool {
	for _, id := range c.ManagerSlackIDs {
		if id == slackUserID {
			return true
		}
	}
	return false
}
