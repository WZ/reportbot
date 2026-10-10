package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultExternalHTTPTimeout = 90 * time.Second
const defaultExternalHTTPTimeoutSeconds = int(defaultExternalHTTPTimeout / time.Second)

type Config struct {
	SlackBotToken string `yaml:"slack_bot_token"`
	SlackAppToken string `yaml:"slack_app_token"`

	GitLabURL            string `yaml:"gitlab_url"`
	GitLabToken          string `yaml:"gitlab_token"`
	GitLabGroupID        string `yaml:"gitlab_group_id"`
	GitLabRefTicketLabel string `yaml:"gitlab_ref_ticket_label"`

	GitHubToken string   `yaml:"github_token"`
	GitHubOrg   string   `yaml:"github_org"`
	GitHubRepos []string `yaml:"github_repos"`

	LLMProvider      string `yaml:"llm_provider"`
	LLMModel         string `yaml:"llm_model"`
	LLMBatchSize     int    `yaml:"llm_batch_size"`
	LLMExampleCount  int    `yaml:"llm_example_count"`
	LLMExampleMaxLen int    `yaml:"llm_example_max_chars"`
	// Reasoning effort sent to OpenAI-compatible providers; empty sends none.
	LLMReasoningEffort string `yaml:"llm_reasoning_effort"`
	AnthropicAPIKey    string `yaml:"anthropic_api_key"`
	OpenAIAPIKey       string `yaml:"openai_api_key"`
	OpenAIBaseURL      string `yaml:"openai_base_url"`

	DecisionModelURL        string  `yaml:"decision_model_url"`
	DecisionModelAPIKey     string  `yaml:"decision_model_api_key"`
	DecisionModelName       string  `yaml:"decision_model_name"`
	DecisionModelConfidence float64 `yaml:"decision_model_confidence"`

	DBPath                     string `yaml:"db_path"`
	ReportOutputDir            string `yaml:"report_output_dir"`
	ReportChannelID            string `yaml:"report_channel_id"`
	ExternalHTTPTimeoutSeconds int    `yaml:"external_http_timeout_seconds"`
	TLSSkipVerify              bool   `yaml:"tls_skip_verify"`

	ManagerSlackIDs []string `yaml:"manager_slack_ids"`
	// AuthorIDs maps owners to Slack user IDs; it is the only way a fetched or imported Work
	// Item gets an owner. Plain keys are report author names (migrate-v2 import only);
	// "gitlab:<id>" / "github:<id>" keys are numeric code-hosting account IDs (fetch only).
	AuthorIDs         map[string]string `yaml:"author_ids"`
	TeamMembers       []string          `yaml:"team_members"`
	NudgeDay          string            `yaml:"nudge_day"`
	NudgeTime         string            `yaml:"nudge_time"`
	AutoFetchSchedule string            `yaml:"auto_fetch_schedule"`
	MondayCutoffTime  string            `yaml:"monday_cutoff_time"`
	Timezone          string            `yaml:"timezone"`
	TeamName          string            `yaml:"team_name"`

	// Web UI
	WebEnabled       bool   `yaml:"web_enabled"`
	WebPort          int    `yaml:"web_port"`
	WebClientID      string `yaml:"web_client_id"`
	WebClientSecret  string `yaml:"web_client_secret"`
	WebSessionSecret string `yaml:"web_session_secret"`
	WebBaseURL       string `yaml:"web_base_url"`

	Location *time.Location `yaml:"-"` // computed from Timezone, not from YAML
}

func LoadConfig() Config {
	var cfg Config

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

	envOverride(&cfg.SlackBotToken, "SLACK_BOT_TOKEN")
	envOverride(&cfg.SlackAppToken, "SLACK_APP_TOKEN")
	envOverride(&cfg.GitLabURL, "GITLAB_URL")
	envOverride(&cfg.GitLabToken, "GITLAB_TOKEN")
	envOverride(&cfg.GitLabGroupID, "GITLAB_GROUP_ID")
	envOverrideAllowEmpty(&cfg.GitLabRefTicketLabel, "GITLAB_REF_TICKET_LABEL")
	envOverride(&cfg.GitHubToken, "GITHUB_TOKEN")
	envOverride(&cfg.GitHubOrg, "GITHUB_ORG")
	if repos := os.Getenv("GITHUB_REPOS"); repos != "" {
		cfg.GitHubRepos = nil
		for _, r := range strings.Split(repos, ",") {
			r = strings.TrimSpace(r)
			if r != "" {
				cfg.GitHubRepos = append(cfg.GitHubRepos, r)
			}
		}
	}
	envOverride(&cfg.LLMProvider, "LLM_PROVIDER")
	envOverride(&cfg.LLMModel, "LLM_MODEL")
	envOverrideInt(&cfg.LLMBatchSize, "LLM_BATCH_SIZE")
	envOverrideInt(&cfg.LLMExampleCount, "LLM_EXAMPLE_COUNT")
	envOverrideInt(&cfg.LLMExampleMaxLen, "LLM_EXAMPLE_MAX_CHARS")
	envOverride(&cfg.LLMReasoningEffort, "LLM_REASONING_EFFORT")
	envOverride(&cfg.AnthropicAPIKey, "ANTHROPIC_API_KEY")
	envOverride(&cfg.OpenAIAPIKey, "OPENAI_API_KEY")
	envOverride(&cfg.OpenAIBaseURL, "OPENAI_BASE_URL")
	envOverride(&cfg.DecisionModelURL, "DECISION_MODEL_URL")
	envOverride(&cfg.DecisionModelAPIKey, "DECISION_MODEL_API_KEY")
	envOverride(&cfg.DecisionModelName, "DECISION_MODEL_NAME")
	envOverrideFloat(&cfg.DecisionModelConfidence, "DECISION_MODEL_CONFIDENCE")
	envOverride(&cfg.DBPath, "DB_PATH")
	envOverride(&cfg.ReportOutputDir, "REPORT_OUTPUT_DIR")
	envOverride(&cfg.ReportChannelID, "REPORT_CHANNEL_ID")
	envOverrideInt(&cfg.ExternalHTTPTimeoutSeconds, "EXTERNAL_HTTP_TIMEOUT_SECONDS")
	envOverrideBool(&cfg.TLSSkipVerify, "TLS_SKIP_VERIFY")
	envOverride(&cfg.TeamName, "TEAM_NAME")
	envOverride(&cfg.NudgeDay, "NUDGE_DAY")
	envOverride(&cfg.NudgeTime, "NUDGE_TIME")
	envOverride(&cfg.AutoFetchSchedule, "AUTO_FETCH_SCHEDULE")
	envOverride(&cfg.MondayCutoffTime, "MONDAY_CUTOFF_TIME")
	envOverride(&cfg.Timezone, "TIMEZONE")
	envOverrideBool(&cfg.WebEnabled, "WEB_ENABLED")
	envOverrideInt(&cfg.WebPort, "WEB_PORT")
	envOverride(&cfg.WebClientID, "WEB_CLIENT_ID")
	envOverride(&cfg.WebClientSecret, "WEB_CLIENT_SECRET")
	envOverride(&cfg.WebSessionSecret, "WEB_SESSION_SECRET")
	envOverride(&cfg.WebBaseURL, "WEB_BASE_URL")

	if raw := os.Getenv("AUTHOR_IDS"); raw != "" {
		cfg.AuthorIDs = nil
		if err := json.Unmarshal([]byte(raw), &cfg.AuthorIDs); err != nil {
			log.Fatalf("invalid AUTHOR_IDS (want a JSON object of name to Slack user ID): %v", err)
		}
	}

	if ids := os.Getenv("MANAGER_SLACK_IDS"); ids != "" {
		cfg.ManagerSlackIDs = nil
		for _, id := range strings.Split(ids, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				cfg.ManagerSlackIDs = append(cfg.ManagerSlackIDs, id)
			}
		}
	}

	if cfg.LLMProvider == "" {
		cfg.LLMProvider = "anthropic"
	}
	if cfg.LLMBatchSize == 0 {
		cfg.LLMBatchSize = 50
	}
	if cfg.DecisionModelConfidence == 0 {
		cfg.DecisionModelConfidence = 0.95
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
	if cfg.OpenAIBaseURL == "" {
		cfg.OpenAIBaseURL = "https://api.openai.com/v1"
	}
	if cfg.ReportOutputDir == "" {
		cfg.ReportOutputDir = "./reports"
	}
	if cfg.ExternalHTTPTimeoutSeconds == 0 {
		cfg.ExternalHTTPTimeoutSeconds = defaultExternalHTTPTimeoutSeconds
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
	if cfg.WebPort == 0 {
		cfg.WebPort = 8080
	}
	if cfg.WebBaseURL == "" {
		cfg.WebBaseURL = fmt.Sprintf("http://localhost:%d", cfg.WebPort)
	}
	if cfg.Timezone == "" {
		cfg.Timezone = "Local"
	}

	required := map[string]string{
		"slack_bot_token":        cfg.SlackBotToken,
		"slack_app_token":        cfg.SlackAppToken,
		"decision_model_url":     cfg.DecisionModelURL,
		"decision_model_api_key": cfg.DecisionModelAPIKey,
		"decision_model_name":    cfg.DecisionModelName,
	}
	for name, val := range required {
		if val == "" {
			log.Fatalf("Required config '%s' is not set (via config.yaml or env var)", name)
		}
	}

	gitlabFields := map[string]string{
		"gitlab_url":      cfg.GitLabURL,
		"gitlab_token":    cfg.GitLabToken,
		"gitlab_group_id": cfg.GitLabGroupID,
	}
	gitlabSet := 0
	for _, v := range gitlabFields {
		if v != "" {
			gitlabSet++
		}
	}
	if gitlabSet > 0 && gitlabSet < len(gitlabFields) {
		for name, val := range gitlabFields {
			if val == "" {
				log.Fatalf("Partial GitLab config: '%s' is not set (all of gitlab_url, gitlab_token, gitlab_group_id are required together)", name)
			}
		}
	}

	if cfg.GitHubToken != "" && cfg.GitHubOrg == "" && len(cfg.GitHubRepos) == 0 {
		log.Fatalf("github_token is set but neither github_org nor github_repos is configured")
	}

	if !cfg.GitLabConfigured() && !cfg.GitHubConfigured() {
		log.Printf("WARNING: Neither GitLab nor GitHub is configured. /fetch will have nothing to fetch.")
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

	if cfg.WebEnabled {
		if cfg.WebSessionSecret == "" {
			log.Fatalf("web_session_secret is required when web_enabled=true (generate with: openssl rand -hex 32)")
		}
		if cfg.WebClientID == "" {
			log.Fatalf("web_client_id is required when web_enabled=true")
		}
		if cfg.WebClientSecret == "" {
			log.Fatalf("web_client_secret is required when web_enabled=true")
		}
	}

	if strings.EqualFold(cfg.Timezone, "Local") {
		cfg.Location = time.Local
	} else {
		loc, err := time.LoadLocation(cfg.Timezone)
		if err != nil {
			log.Fatalf("invalid timezone '%s': %v", cfg.Timezone, err)
		}
		cfg.Location = loc
	}

	if _, _, err := parseClock(cfg.MondayCutoffTime); err != nil {
		log.Fatalf("invalid monday_cutoff_time '%s': %v", cfg.MondayCutoffTime, err)
	}
	if cfg.LLMBatchSize < 1 {
		log.Fatalf("invalid llm_batch_size '%d': must be >= 1", cfg.LLMBatchSize)
	}
	if cfg.DecisionModelConfidence <= 0 || cfg.DecisionModelConfidence >= 1 {
		log.Fatalf("invalid decision_model_confidence '%f': must be between 0 and 1 (exclusive)", cfg.DecisionModelConfidence)
	}
	if cfg.LLMExampleCount < 0 {
		log.Fatalf("invalid llm_example_count '%d': must be >= 0", cfg.LLMExampleCount)
	}
	if cfg.LLMExampleMaxLen < 20 {
		log.Fatalf("invalid llm_example_max_chars '%d': must be >= 20", cfg.LLMExampleMaxLen)
	}
	authorIDs, err := normalizeAuthorIDs(cfg.AuthorIDs)
	if err != nil {
		log.Fatalf("invalid author_ids: %v", err)
	}
	cfg.AuthorIDs = authorIDs

	switch cfg.LLMReasoningEffort {
	case "", "low", "medium", "high":
	default:
		log.Fatalf("invalid llm_reasoning_effort '%s': must be empty, low, medium or high", cfg.LLMReasoningEffort)
	}
	if cfg.ExternalHTTPTimeoutSeconds < 5 {
		log.Fatalf("invalid external_http_timeout_seconds '%d': must be >= 5", cfg.ExternalHTTPTimeoutSeconds)
	}
	if cfg.OpenAIBaseURL != "" {
		cfg.OpenAIBaseURL = strings.TrimRight(cfg.OpenAIBaseURL, "/")
	}
	cfg.DecisionModelURL = strings.TrimRight(cfg.DecisionModelURL, "/")

	return cfg
}

func envOverride(field *string, envKey string) {
	if val := os.Getenv(envKey); val != "" {
		*field = val
	}
}

func envOverrideAllowEmpty(field *string, envKey string) {
	if val, ok := os.LookupEnv(envKey); ok {
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

func envOverrideBool(field *bool, envKey string) {
	if val := os.Getenv(envKey); val != "" {
		*field = strings.EqualFold(val, "true") || val == "1"
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

func (c Config) IsManagerID(userID string) bool {
	for _, id := range c.ManagerSlackIDs {
		if strings.TrimSpace(id) == userID {
			return true
		}
	}
	return false
}

var (
	slackUserIDRe = regexp.MustCompile(`^[UW][A-Z0-9]{8,}$`)
	accountKeyRe  = regexp.MustCompile(`^(gitlab|github):[0-9]+$`)
)

func IsSlackUserID(s string) bool { return slackUserIDRe.MatchString(s) }

func authorKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// normalizeAuthorIDs folds keys for lookup and rejects values that are not Slack user IDs.
func normalizeAuthorIDs(in map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for name, id := range in {
		key, id := authorKey(name), strings.TrimSpace(id)
		if key == "" {
			return nil, fmt.Errorf("empty author name")
		}
		if isAccountKey(key) && !accountKeyRe.MatchString(key) {
			return nil, fmt.Errorf("%q: account keys are gitlab:<numeric id> or github:<numeric id>", name)
		}
		if !IsSlackUserID(id) {
			return nil, fmt.Errorf("%q maps to %q, which is not a Slack user ID", name, id)
		}
		if prev, ok := out[key]; ok && prev != id {
			return nil, fmt.Errorf("%q maps to two Slack user IDs", name)
		}
		out[key] = id
	}
	return out, nil
}

func isAccountKey(key string) bool {
	return strings.HasPrefix(key, "gitlab:") || strings.HasPrefix(key, "github:")
}

// AuthorID returns the Slack user ID configured for a report author name, or "".
// Account keys never match here, so a name cannot be used to reach them.
func (c Config) AuthorID(name string) string {
	key := authorKey(name)
	if isAccountKey(key) {
		return ""
	}
	return c.AuthorIDs[key]
}

// AccountAuthorID returns the Slack user ID configured for a code-hosting account
// ("gitlab" or "github") by its immutable numeric ID, or "".
func (c Config) AccountAuthorID(host string, accountID int64) string {
	if accountID <= 0 {
		return ""
	}
	return c.AuthorIDs[fmt.Sprintf("%s:%d", host, accountID)]
}

func (c Config) GitLabConfigured() bool {
	return c.GitLabURL != "" && c.GitLabToken != "" && c.GitLabGroupID != ""
}

func (c Config) GitHubConfigured() bool {
	return c.GitHubToken != "" && (c.GitHubOrg != "" || len(c.GitHubRepos) > 0)
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
