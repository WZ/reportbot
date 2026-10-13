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

// GitLabSource is one GitLab instance to fetch: either a group's merge requests
// (group_id) or a project branch's commits (project + branch), never both.
// name qualifies author_ids keys ("gitlab@<name>:<id or username>") and must be
// unique. Token supports ${ENV_VAR} interpolation from the process environment.
type GitLabSource struct {
	Name           string `yaml:"name"`
	URL            string `yaml:"url"`
	Token          string `yaml:"token"`
	GroupID        string `yaml:"group_id"`
	Project        string `yaml:"project"`
	Branch         string `yaml:"branch"`
	RefTicketLabel string `yaml:"ref_ticket_label"`
	Default        bool   `yaml:"default"`
}

// Label names the source in fetch summaries and errors: the name for a group
// source, name/project/branch for a commit source.
func (s GitLabSource) Label() string {
	if s.GroupID == "" {
		return fmt.Sprintf("%s/%s/%s", s.Name, s.Project, s.Branch)
	}
	return s.Name + " (MRs)"
}

type Config struct {
	SlackBotToken string `yaml:"slack_bot_token"`
	SlackAppToken string `yaml:"slack_app_token"`

	// GitLabSources lists the GitLab instances to fetch: each entry is a group
	// (merge requests) or a project branch (commits), with its own token.
	GitLabSources []GitLabSource `yaml:"gitlab_sources"`

	GitHubToken string   `yaml:"github_token"`
	GitHubOrg   string   `yaml:"github_org"`
	GitHubRepos []string `yaml:"github_repos"`
	// GitHubRefTicketLabel reads tickets from PR descriptions, as GitLab sources do
	// per-source; GitHub has a single token so it keeps one label.
	GitHubRefTicketLabel string `yaml:"github_ref_ticket_label"`

	// The LLM is optional; only Structure Proposals use it. Empty llm_provider means none.
	LLMProvider string `yaml:"llm_provider"`
	LLMModel    string `yaml:"llm_model"`
	// Reasoning effort sent to OpenAI-compatible providers; empty sends none.
	LLMReasoningEffort string `yaml:"llm_reasoning_effort"`
	AnthropicAPIKey    string `yaml:"anthropic_api_key"`
	OpenAIAPIKey       string `yaml:"openai_api_key"`
	OpenAIBaseURL      string `yaml:"openai_base_url"`

	DecisionModelURL        string  `yaml:"decision_model_url"`
	DecisionModelAPIKey     string  `yaml:"decision_model_api_key"`
	DecisionModelName       string  `yaml:"decision_model_name"`
	DecisionModelConfidence float64 `yaml:"decision_model_confidence"`
	// Similar past Work Items (and their Sections) the decision model sees when filing.
	SimilarItemCount int `yaml:"similar_item_count"`

	DBPath                     string `yaml:"db_path"`
	ReportOutputDir            string `yaml:"report_output_dir"`
	ReportChannelID            string `yaml:"report_channel_id"`
	ExternalHTTPTimeoutSeconds int    `yaml:"external_http_timeout_seconds"`
	TLSSkipVerify              bool   `yaml:"tls_skip_verify"`

	ManagerSlackIDs []string `yaml:"manager_slack_ids"`
	// AuthorIDs maps owners to Slack user IDs; it is the only way a fetched or imported Work
	// Item gets an owner. Plain keys are report author names (migrate-v2 import only);
	// "gitlab:<id or username>" / "github:<id or login>" keys are code-hosting accounts (fetch only).
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
	var legacyKeys []string
	if data, err := os.ReadFile(configPath); err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			log.Fatalf("Error parsing %s: %v", configPath, err)
		}
		var raw map[string]any
		if err := yaml.Unmarshal(data, &raw); err == nil {
			legacyKeys = gitlabLegacyKeys(raw)
		}
		log.Printf("Loaded config from %s", configPath)
	}

	envOverride(&cfg.SlackBotToken, "SLACK_BOT_TOKEN")
	envOverride(&cfg.SlackAppToken, "SLACK_APP_TOKEN")
	envOverride(&cfg.GitHubToken, "GITHUB_TOKEN")
	envOverride(&cfg.GitHubOrg, "GITHUB_ORG")
	envOverride(&cfg.GitHubRefTicketLabel, "GITHUB_REF_TICKET_LABEL")
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
	envOverrideInt(&cfg.SimilarItemCount, "SIMILAR_ITEM_COUNT")
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

	if cfg.DecisionModelConfidence == 0 {
		cfg.DecisionModelConfidence = 0.85
	}
	if cfg.SimilarItemCount == 0 {
		cfg.SimilarItemCount = 30
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

	if len(legacyKeys) > 0 && len(cfg.GitLabSources) == 0 {
		log.Fatalf("The %s config keys were removed; use gitlab_sources entries instead:\n"+
			"  gitlab_sources:\n"+
			"    - name: <short-name>\n"+
			"      url: <instance-url>\n"+
			"      token: ${GITLAB_TOKEN}   # ${ENV_VAR} placeholders resolve from the environment\n"+
			"      group_id: <group>\n"+
			"      ref_ticket_label: <label>  # optional\n"+
			"      default: true              # exactly one source may set this",
			strings.Join(legacyKeys, ", "))
	}
	for i := range cfg.GitLabSources {
		// Interpolate and trim first, so validation sees the values fetch will use.
		cfg.GitLabSources[i].Token = strings.TrimSpace(interpolateEnv(cfg.GitLabSources[i].Token))
		cfg.GitLabSources[i].Name = strings.TrimSpace(cfg.GitLabSources[i].Name)
		cfg.GitLabSources[i].URL = strings.TrimSpace(cfg.GitLabSources[i].URL)
		cfg.GitLabSources[i].GroupID = strings.TrimSpace(cfg.GitLabSources[i].GroupID)
		cfg.GitLabSources[i].Project = strings.TrimSpace(cfg.GitLabSources[i].Project)
		cfg.GitLabSources[i].Branch = strings.TrimSpace(cfg.GitLabSources[i].Branch)
		cfg.GitLabSources[i].RefTicketLabel = strings.TrimSpace(cfg.GitLabSources[i].RefTicketLabel)
		if err := validateGitLabSource(cfg.GitLabSources[i]); err != nil {
			log.Fatalf("gitlab_sources[%d]: %v", i, err)
		}
	}
	if err := validateGitLabSourceNames(cfg.GitLabSources); err != nil {
		log.Fatalf("invalid gitlab_sources: %v", err)
	}
	if len(legacyKeys) > 0 && len(cfg.GitLabSources) > 0 {
		log.Printf("WARNING: ignoring removed %s keys; gitlab_sources is in effect", strings.Join(legacyKeys, ", "))
	}

	if cfg.GitHubToken != "" && cfg.GitHubOrg == "" && len(cfg.GitHubRepos) == 0 {
		log.Fatalf("github_token is set but neither github_org nor github_repos is configured")
	}

	if !cfg.GitLabConfigured() && !cfg.GitHubConfigured() {
		log.Printf("WARNING: Neither GitLab nor GitHub is configured. /fetch will have nothing to fetch.")
	}

	switch cfg.LLMProvider {
	case "":
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
	if cfg.DecisionModelConfidence <= 0 || cfg.DecisionModelConfidence >= 1 {
		log.Fatalf("invalid decision_model_confidence '%f': must be between 0 and 1 (exclusive)", cfg.DecisionModelConfidence)
	}
	if cfg.SimilarItemCount < 1 {
		log.Fatalf("invalid similar_item_count '%d': must be >= 1", cfg.SimilarItemCount)
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
	accountKeyRe  = regexp.MustCompile(`^(gitlab(?:@[a-z0-9_-]+)?|github):[a-z0-9_.-]+$`)
	digitsRe      = regexp.MustCompile(`^[0-9]+$`)
	usernameRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
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
			return nil, fmt.Errorf("%q: account keys are gitlab:<numeric id or username> or github:<numeric id or login>; usernames use only letters, digits, '_', '-' and '.'", name)
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

// isAccountKey reports whether a folded author_ids key looks like a code-hosting
// account key ("gitlab:", "gitlab@<server>:" or "github:"), so it can be checked
// against accountKeyRe instead of treated as a plain report-name key.
func isAccountKey(key string) bool {
	return strings.HasPrefix(key, "gitlab:") || strings.HasPrefix(key, "gitlab@") || strings.HasPrefix(key, "github:")
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

// IsAccountUsername reports whether s can be an author_ids username key; all digits is a numeric ID.
func IsAccountUsername(s string) bool { return usernameRe.MatchString(s) && !digitsRe.MatchString(s) }

// AccountAuthorID returns the Slack user ID for a code-hosting account ("gitlab" or
// "github"): its numeric-ID key wins, else its username key (exact, case-insensitive);
// "" if neither. warn is set when both keys match but map to different Slack user IDs.
//
// For GitLab with several sources, server is the source name: keys are looked up as
// "gitlab@<server>:<id or username>" first, then the unqualified form resolves only
// against the default source (numeric IDs and usernames are per-instance, so an
// unqualified key must never match a non-default server). server is empty for GitHub.
func (c Config) AccountAuthorID(host, server string, accountID int64, username string) (slackID, warn string) {
	prefix := host
	if host == "gitlab" {
		switch {
		case server != "":
			prefix = host + "@" + strings.ToLower(server)
		case c.defaultGitLabSource() == "":
			// No default source: nothing unqualified can safely resolve.
			return "", ""
		}
	}
	// The unqualified form resolves only for the default source's server;
	// a named non-default server binds through its qualified keys alone.
	unqualifiedOK := host != "gitlab" || server == "" || strings.EqualFold(server, c.defaultGitLabSource())
	var idKey, byID, userKey, byUser string
	if accountID > 0 {
		idKey = fmt.Sprintf("%s:%d", prefix, accountID)
		byID = c.AuthorIDs[idKey]
		if byID == "" && prefix != host && unqualifiedOK {
			idKey = fmt.Sprintf("%s:%d", host, accountID)
			byID = c.AuthorIDs[idKey]
		}
	}
	if username = strings.TrimSpace(username); IsAccountUsername(username) {
		userKey = prefix + ":" + strings.ToLower(username)
		byUser = c.AuthorIDs[userKey]
		if byUser == "" && prefix != host && unqualifiedOK {
			userKey = host + ":" + strings.ToLower(username)
			byUser = c.AuthorIDs[userKey]
		}
	}
	if byID == "" {
		return byUser, ""
	}
	if byUser != "" && byUser != byID {
		warn = fmt.Sprintf("author_ids %q and %q map to different Slack users; using %q", idKey, userKey, idKey)
	}
	return byID, warn
}

// defaultGitLabSource returns the name of the source with default: true, or "".
func (c Config) defaultGitLabSource() string {
	for _, s := range c.GitLabSources {
		if s.Default {
			return s.Name
		}
	}
	return ""
}

// LLMConfigured reports whether an LLM is set up for Structure Proposals.
func (c Config) LLMConfigured() bool { return c.LLMProvider != "" }

func (c Config) GitLabConfigured() bool { return len(c.GitLabSources) > 0 }

var (
	sourceNameRe     = regexp.MustCompile(`^[a-z0-9_-]+$`)
	envPlaceholderRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

// gitlabLegacyKeys lists the removed flat gitlab_* keys present in a raw YAML document.
func gitlabLegacyKeys(raw map[string]any) []string {
	var found []string
	for _, key := range []string{"gitlab_url", "gitlab_token", "gitlab_group_id", "gitlab_ref_ticket_label"} {
		if v, ok := raw[key]; ok {
			if s, ok := v.(string); !ok || strings.TrimSpace(s) != "" {
				found = append(found, key)
			}
		}
	}
	return found
}

// validateGitLabSource checks one gitlab_sources entry: url and token set, a name
// that can qualify author_ids keys, and exactly one of group_id / project+branch.
func validateGitLabSource(s GitLabSource) error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("name is required (it qualifies author_ids keys like gitlab@name:<id>)")
	}
	if !sourceNameRe.MatchString(s.Name) {
		return fmt.Errorf("name %q must use only lowercase letters, digits, '-' and '_'", s.Name)
	}
	if strings.TrimSpace(s.URL) == "" {
		return fmt.Errorf("url is required")
	}
	if strings.TrimSpace(s.Token) == "" {
		return fmt.Errorf("token is required (${ENV_VAR} placeholders resolve from the environment)")
	}
	hasGroup := strings.TrimSpace(s.GroupID) != ""
	hasProject := strings.TrimSpace(s.Project) != ""
	hasBranch := strings.TrimSpace(s.Branch) != ""
	switch {
	case hasGroup && (hasProject || hasBranch):
		return fmt.Errorf("group_id and project/branch are mutually exclusive (group fetches merge requests, project+branch fetches commits)")
	case !hasGroup && !hasProject && !hasBranch:
		return fmt.Errorf("set either group_id (merge requests) or project and branch (commits)")
	case !hasGroup && (!hasProject || !hasBranch):
		// ref_name= empty would silently fetch the project's default branch.
		return fmt.Errorf("branch sources need both project and branch (an empty ref_name fetches the default branch)")
	}
	return nil
}

// validateGitLabSourceNames enforces unique names and exactly one default source
// (the default resolves unqualified gitlab:<id|username> author_ids keys).
func validateGitLabSourceNames(sources []GitLabSource) error {
	seen := map[string]bool{}
	defaults := 0
	for _, s := range sources {
		if seen[s.Name] {
			return fmt.Errorf("duplicate name %q", s.Name)
		}
		seen[s.Name] = true
		if s.Default {
			defaults++
		}
	}
	switch {
	case len(sources) > 0 && defaults == 0:
		return fmt.Errorf("exactly one source must set default: true (it resolves unqualified gitlab:<id|username> author_ids keys)")
	case defaults > 1:
		return fmt.Errorf("only one source may set default: true, got %d", defaults)
	}
	return nil
}

// interpolateEnv resolves ${ENV_VAR} placeholders in a config value from the
// process environment; an unset variable becomes empty.
func interpolateEnv(s string) string {
	return envPlaceholderRe.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(envPlaceholderRe.FindStringSubmatch(m)[1])
	})
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
