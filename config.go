package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	SlackBotToken string `yaml:"slack_bot_token"`
	SlackAppToken string `yaml:"slack_app_token"`

	GitLabURL     string `yaml:"gitlab_url"`
	GitLabToken   string `yaml:"gitlab_token"`
	GitLabGroupID string `yaml:"gitlab_group_id"`

	LLMProvider      string  `yaml:"llm_provider"`
	LLMModel         string  `yaml:"llm_model"`
	LLMBatchSize     int     `yaml:"llm_batch_size"`
	LLMConfidence    float64 `yaml:"llm_confidence_threshold"`
	LLMExampleCount  int     `yaml:"llm_example_count"`
	LLMExampleMaxLen int     `yaml:"llm_example_max_chars"`
	LLMGlossaryPath  string  `yaml:"llm_glossary_path"`
	AnthropicAPIKey  string  `yaml:"anthropic_api_key"`
	OpenAIAPIKey     string  `yaml:"openai_api_key"`

	DBPath          string `yaml:"db_path"`
	ReportOutputDir string `yaml:"report_output_dir"`
	ReportChannelID string `yaml:"report_channel_id"`

	Managers         []string `yaml:"manager"`
	TeamMembers      []string `yaml:"team_members"`
	NudgeDay         string   `yaml:"nudge_day"`
	NudgeTime        string   `yaml:"nudge_time"`
	MondayCutoffTime string   `yaml:"monday_cutoff_time"`
	Timezone         string   `yaml:"timezone"`
	TeamName         string   `yaml:"team_name"`
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
	envOverrideInt(&cfg.LLMBatchSize, "LLM_BATCH_SIZE")
	envOverrideFloat(&cfg.LLMConfidence, "LLM_CONFIDENCE_THRESHOLD")
	envOverrideInt(&cfg.LLMExampleCount, "LLM_EXAMPLE_COUNT")
	envOverrideInt(&cfg.LLMExampleMaxLen, "LLM_EXAMPLE_MAX_CHARS")
	envOverride(&cfg.LLMGlossaryPath, "LLM_GLOSSARY_PATH")
	envOverride(&cfg.AnthropicAPIKey, "ANTHROPIC_API_KEY")
	envOverride(&cfg.OpenAIAPIKey, "OPENAI_API_KEY")
	envOverride(&cfg.DBPath, "DB_PATH")
	envOverride(&cfg.ReportOutputDir, "REPORT_OUTPUT_DIR")
	envOverride(&cfg.ReportChannelID, "REPORT_CHANNEL_ID")
	envOverride(&cfg.TeamName, "TEAM_NAME")
	envOverride(&cfg.NudgeDay, "NUDGE_DAY")
	envOverride(&cfg.NudgeTime, "NUDGE_TIME")
	envOverride(&cfg.MondayCutoffTime, "MONDAY_CUTOFF_TIME")
	envOverride(&cfg.Timezone, "TIMEZONE")

	if names := os.Getenv("MANAGER"); names != "" {
		cfg.Managers = nil
		for _, name := range strings.Split(names, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				cfg.Managers = append(cfg.Managers, name)
			}
		}
	}

	// Defaults
	if cfg.LLMProvider == "" {
		cfg.LLMProvider = "anthropic"
	}
	if cfg.LLMBatchSize == 0 {
		cfg.LLMBatchSize = 50
	}
	if cfg.LLMConfidence == 0 {
		cfg.LLMConfidence = 0.70
	}
	if cfg.LLMExampleCount == 0 {
		cfg.LLMExampleCount = 20
	}
	if cfg.LLMExampleMaxLen == 0 {
		cfg.LLMExampleMaxLen = 140
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
	if cfg.MondayCutoffTime == "" {
		cfg.MondayCutoffTime = "12:00"
	}
	if cfg.TeamName == "" {
		cfg.TeamName = "My Team"
	}
	if cfg.Timezone == "" {
		cfg.Timezone = "Local"
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

	if strings.EqualFold(cfg.Timezone, "Local") {
		cfg.Timezone = time.Local.String()
	} else {
		loc, err := time.LoadLocation(cfg.Timezone)
		if err != nil {
			log.Fatalf("invalid timezone '%s': %v", cfg.Timezone, err)
		}
		time.Local = loc
	}

	if _, _, err := parseClock(cfg.MondayCutoffTime); err != nil {
		log.Fatalf("invalid monday_cutoff_time '%s': %v", cfg.MondayCutoffTime, err)
	}
	if cfg.LLMBatchSize < 1 {
		log.Fatalf("invalid llm_batch_size '%d': must be >= 1", cfg.LLMBatchSize)
	}
	if cfg.LLMConfidence < 0 || cfg.LLMConfidence > 1 {
		log.Fatalf("invalid llm_confidence_threshold '%f': must be between 0 and 1", cfg.LLMConfidence)
	}
	if cfg.LLMExampleCount < 0 {
		log.Fatalf("invalid llm_example_count '%d': must be >= 0", cfg.LLMExampleCount)
	}
	if cfg.LLMExampleMaxLen < 20 {
		log.Fatalf("invalid llm_example_max_chars '%d': must be >= 20", cfg.LLMExampleMaxLen)
	}
	if cfg.LLMGlossaryPath != "" {
		if _, err := LoadLLMGlossary(cfg.LLMGlossaryPath); err != nil {
			log.Fatalf("invalid llm_glossary_path '%s': %v", cfg.LLMGlossaryPath, err)
		}
	}

	return cfg
}

func envOverride(field *string, envKey string) {
	if val := os.Getenv(envKey); val != "" {
		*field = val
	}
}

func envOverrideInt(field *int, envKey string) {
	if val := os.Getenv(envKey); val != "" {
		parsed, err := strconv.Atoi(val)
		if err != nil {
			log.Fatalf("invalid %s '%s': %v", envKey, val, err)
		}
		*field = parsed
	}
}

func envOverrideFloat(field *float64, envKey string) {
	if val := os.Getenv(envKey); val != "" {
		parsed, err := strconv.ParseFloat(val, 64)
		if err != nil {
			log.Fatalf("invalid %s '%s': %v", envKey, val, err)
		}
		*field = parsed
	}
}

func (c Config) IsManagerName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, manager := range c.Managers {
		if strings.ToLower(strings.TrimSpace(manager)) == name {
			return true
		}
	}
	return false
}

func parseClock(s string) (int, int, error) {
	var hour, min int
	_, err := fmt.Sscanf(s, "%d:%d", &hour, &min)
	if err != nil {
		return 0, 0, err
	}
	if hour < 0 || hour > 23 || min < 0 || min > 59 {
		return 0, 0, fmt.Errorf("time out of range: %02d:%02d", hour, min)
	}
	return hour, min, nil
}
