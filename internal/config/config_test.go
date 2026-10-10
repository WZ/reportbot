package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func setMinimalValidConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("TIMEZONE", "UTC")
	t.Setenv("DECISION_MODEL_URL", "https://api.compat.example/")
	t.Setenv("DECISION_MODEL_API_KEY", "dm-test")
	t.Setenv("DECISION_MODEL_NAME", "test-model")
}

func TestLoadConfigFromEnvWithDefaults(t *testing.T) {
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	setMinimalValidConfigEnv(t)
	t.Setenv("MANAGER_SLACK_IDS", "U12345, U67890")

	cfg := LoadConfig()

	if cfg.SlackBotToken != "xoxb-test" {
		t.Fatalf("unexpected slack bot token: %q", cfg.SlackBotToken)
	}
	if cfg.SlackAppToken != "xapp-test" {
		t.Fatalf("unexpected slack app token: %q", cfg.SlackAppToken)
	}
	if cfg.LLMProvider != "openai" {
		t.Fatalf("unexpected provider: %q", cfg.LLMProvider)
	}
	if cfg.DBPath != "./reportbot.db" {
		t.Fatalf("unexpected db path default: %q", cfg.DBPath)
	}
	if cfg.OpenAIBaseURL != "https://api.openai.com/v1" {
		t.Fatalf("unexpected OpenAI base URL default: %q", cfg.OpenAIBaseURL)
	}
	if cfg.ReportOutputDir != "./reports" {
		t.Fatalf("unexpected report output dir default: %q", cfg.ReportOutputDir)
	}
	if cfg.ExternalHTTPTimeoutSeconds != int(defaultExternalHTTPTimeout/time.Second) {
		t.Fatalf("unexpected external HTTP timeout default: %d", cfg.ExternalHTTPTimeoutSeconds)
	}
	if cfg.TeamName != "My Team" {
		t.Fatalf("unexpected team name default: %q", cfg.TeamName)
	}
	if cfg.GitLabRefTicketLabel != "" {
		t.Fatalf("unexpected gitlab ref ticket label default: %q", cfg.GitLabRefTicketLabel)
	}
	if cfg.DecisionModelConfidence != 0.70 {
		t.Fatalf("unexpected decision model confidence default: %v", cfg.DecisionModelConfidence)
	}
	if cfg.DecisionModelURL != "https://api.compat.example" || cfg.DecisionModelName != "test-model" || cfg.DecisionModelAPIKey != "dm-test" {
		t.Fatalf("unexpected decision model config: %+v", cfg)
	}
	if cfg.Location == nil || cfg.Location.String() != "UTC" {
		t.Fatalf("unexpected location: %v", cfg.Location)
	}
	if len(cfg.ManagerSlackIDs) != 2 {
		t.Fatalf("expected 2 manager IDs, got %d", len(cfg.ManagerSlackIDs))
	}
}

func TestLoadConfigYAMLAndEnvOverride(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := `
slack_bot_token: "yaml-bot"
slack_app_token: "yaml-app"
llm_provider: "anthropic"
anthropic_api_key: "yaml-anthropic"
team_name: "YAML Team"
timezone: "America/Los_Angeles"
db_path: "/tmp/yaml.db"
report_output_dir: "/tmp/yaml-reports"
external_http_timeout_seconds: 75
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("CONFIG_PATH", cfgPath)
	t.Setenv("DECISION_MODEL_URL", "https://api.compat.example")
	t.Setenv("DECISION_MODEL_API_KEY", "dm-test")
	t.Setenv("DECISION_MODEL_NAME", "test-model")
	t.Setenv("DECISION_MODEL_CONFIDENCE", "0.55")
	t.Setenv("LLM_REASONING_EFFORT", "low")
	t.Setenv("AUTHOR_IDS", `{" Dana Lee ": "U0123ABCD", "dana.lee": "U0123ABCD"}`)
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-env")
	t.Setenv("OPENAI_BASE_URL", "https://api.compat.example/v1/")
	t.Setenv("TEAM_NAME", "Env Team")
	t.Setenv("DB_PATH", "/tmp/env.db")
	t.Setenv("EXTERNAL_HTTP_TIMEOUT_SECONDS", "120")
	t.Setenv("GITLAB_REF_TICKET_LABEL", "Jira")

	cfg := LoadConfig()

	if cfg.TeamName != "Env Team" {
		t.Fatalf("expected team name from env override, got %q", cfg.TeamName)
	}
	if cfg.LLMProvider != "openai" {
		t.Fatalf("expected provider from env override, got %q", cfg.LLMProvider)
	}
	if cfg.OpenAIAPIKey != "sk-env" {
		t.Fatalf("expected openai key from env override")
	}
	if cfg.OpenAIBaseURL != "https://api.compat.example/v1" {
		t.Fatalf("expected openai base URL from env override, got %q", cfg.OpenAIBaseURL)
	}
	if cfg.DBPath != "/tmp/env.db" {
		t.Fatalf("expected db path from env override, got %q", cfg.DBPath)
	}
	if cfg.ReportOutputDir != "/tmp/yaml-reports" {
		t.Fatalf("expected report output dir from yaml, got %q", cfg.ReportOutputDir)
	}
	if cfg.ExternalHTTPTimeoutSeconds != 120 {
		t.Fatalf("expected external HTTP timeout from env override, got %d", cfg.ExternalHTTPTimeoutSeconds)
	}
	if cfg.GitLabRefTicketLabel != "Jira" {
		t.Fatalf("expected ticket field label from env override, got %q", cfg.GitLabRefTicketLabel)
	}
	if cfg.AuthorID("dana lee") != "U0123ABCD" || cfg.AuthorID("DANA.LEE") != "U0123ABCD" || cfg.AuthorID("Dana") != "" {
		t.Fatalf("author_ids lookup is exact after trimming and case-folding: %v", cfg.AuthorIDs)
	}
	if cfg.LLMReasoningEffort != "low" {
		t.Fatalf("expected reasoning effort from env override, got %q", cfg.LLMReasoningEffort)
	}
	if cfg.DecisionModelConfidence != 0.55 {
		t.Fatalf("expected decision model confidence from env override, got %v", cfg.DecisionModelConfidence)
	}
}

func TestLoadConfigAllowsEmptyGitLabRefTicketLabelEnv(t *testing.T) {
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	setMinimalValidConfigEnv(t)
	t.Setenv("GITLAB_REF_TICKET_LABEL", "")

	cfg := LoadConfig()
	if cfg.GitLabRefTicketLabel != "" {
		t.Fatalf("expected empty gitlab ref ticket label, got %q", cfg.GitLabRefTicketLabel)
	}
}

func TestParseClock(t *testing.T) {
	hour, min, err := parseClock("09:45")
	if err != nil {
		t.Fatalf("parseClock returned error: %v", err)
	}
	if hour != 9 || min != 45 {
		t.Fatalf("unexpected clock parse result: %02d:%02d", hour, min)
	}

	if _, _, err := parseClock("24:00"); err == nil {
		t.Fatal("expected parseClock to fail for out-of-range hour")
	}
	if _, _, err := parseClock("bad"); err == nil {
		t.Fatal("expected parseClock to fail for malformed input")
	}
}

func TestEnvOverrideHelpers(t *testing.T) {
	s := "initial"
	t.Setenv("RB_TEST_STR", "value")
	envOverride(&s, "RB_TEST_STR")
	if s != "value" {
		t.Fatalf("envOverride failed, got %q", s)
	}

	i := 1
	t.Setenv("RB_TEST_INT", "42")
	envOverrideInt(&i, "RB_TEST_INT")
	if i != 42 {
		t.Fatalf("envOverrideInt failed, got %d", i)
	}

	f := 0.1
	t.Setenv("RB_TEST_FLOAT", "0.75")
	envOverrideFloat(&f, "RB_TEST_FLOAT")
	if f != 0.75 {
		t.Fatalf("envOverrideFloat failed, got %f", f)
	}

	b := false
	t.Setenv("RB_TEST_BOOL", "1")
	envOverrideBool(&b, "RB_TEST_BOOL")
	if !b {
		t.Fatalf("envOverrideBool failed, got %v", b)
	}
}

func TestLoadConfigFatalCases(t *testing.T) {
	if os.Getenv("TEST_INVALID_TZ_FATAL") == "1" {
		_ = os.Setenv("CONFIG_PATH", filepath.Join(os.TempDir(), "no-config.yaml"))
		_ = os.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
		_ = os.Setenv("SLACK_APP_TOKEN", "xapp-test")
		_ = os.Setenv("LLM_PROVIDER", "openai")
		_ = os.Setenv("OPENAI_API_KEY", "sk-test")
		_ = os.Setenv("DECISION_MODEL_URL", "https://api.compat.example")
		_ = os.Setenv("DECISION_MODEL_API_KEY", "dm-test")
		_ = os.Setenv("DECISION_MODEL_NAME", "test-model")
		_ = os.Setenv("TIMEZONE", os.Getenv("TEST_FATAL_TZ"))
		_ = os.Setenv("DECISION_MODEL_CONFIDENCE", os.Getenv("TEST_FATAL_CONFIDENCE"))
		_ = os.Setenv("LLM_REASONING_EFFORT", os.Getenv("TEST_FATAL_EFFORT"))
		_ = os.Setenv("AUTHOR_IDS", os.Getenv("TEST_FATAL_AUTHOR_IDS"))
		if os.Getenv("TEST_FATAL_DROP_DECISION_URL") == "1" {
			_ = os.Unsetenv("DECISION_MODEL_URL")
		}
		LoadConfig()
		return
	}

	for name, env := range map[string][]string{
		"invalid timezone":           {"TEST_FATAL_TZ=Mars/Colony"},
		"confidence out of range":    {"TEST_FATAL_TZ=UTC", "TEST_FATAL_CONFIDENCE=1.5"},
		"missing decision model url": {"TEST_FATAL_TZ=UTC", "TEST_FATAL_DROP_DECISION_URL=1"},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=TestLoadConfigFatalCases")
		cmd.Env = append(append(os.Environ(), "TEST_INVALID_TZ_FATAL=1"), env...)
		err := cmd.Run()
		if err == nil {
			t.Fatalf("%s: expected subprocess to exit with failure", name)
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("%s: expected ExitError, got: %v", name, err)
		}
	}
}

func TestNormalizeAuthorIDs(t *testing.T) {
	if _, err := normalizeAuthorIDs(map[string]string{"a": "u0123abcd"}); err == nil {
		t.Fatal("lower-case values are not Slack user IDs")
	}
	if _, err := normalizeAuthorIDs(map[string]string{"A": "U0123ABCD", "a": "U9999ZZZZ"}); err == nil {
		t.Fatal("one author cannot map to two IDs")
	}
	if got, err := normalizeAuthorIDs(map[string]string{" A ": "U0123ABCD"}); err != nil || got["a"] != "U0123ABCD" {
		t.Fatalf("unexpected %v %v", got, err)
	}
}
