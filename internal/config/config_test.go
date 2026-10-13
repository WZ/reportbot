package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	if cfg.DecisionModelConfidence != 0.85 {
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
	for _, bad := range []string{"gitlab:bad name", "gitlab:", "github:", "gitlab:a<b", "github:a/b"} {
		if _, err := normalizeAuthorIDs(map[string]string{bad: "U0123ABCD"}); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
	for _, good := range []string{"gitlab:yfma", "gitlab:24", "github:octo-cat", "gitlab:first.last_2"} {
		if _, err := normalizeAuthorIDs(map[string]string{good: "U0123ABCD"}); err != nil {
			t.Fatalf("%q must be accepted: %v", good, err)
		}
		if _, err := normalizeAuthorIDs(map[string]string{good: "yfma"}); err == nil {
			t.Fatalf("%q: a value that is not a Slack user ID must be rejected", good)
		}
	}
	got, err := normalizeAuthorIDs(map[string]string{"GitLab:42": "U0123ABCD", "github:7": "U0777ABCD"})
	if err != nil {
		t.Fatal(err)
	}
	c := Config{AuthorIDs: got, GitLabSources: []GitLabSource{{Name: "dops", Default: true}}}
	if id, _ := c.AccountAuthorID("gitlab", "", 42, ""); id != "U0123ABCD" {
		t.Fatal("account keys bind by host and numeric ID")
	}
	if id, _ := c.AccountAuthorID("github", "", 7, ""); id != "U0777ABCD" {
		t.Fatal("account keys bind by host and numeric ID")
	}
	if id, _ := c.AccountAuthorID("github", "", 42, ""); id != "" {
		t.Fatal("account keys are per host")
	}
	if c.AuthorID("gitlab:42") != "" {
		t.Fatal("a report name never reaches an account key")
	}
	// A plain name starting with a host word stays a plain name, not a broken account key.
	got2, err := normalizeAuthorIDs(map[string]string{"Github Chen": "U0123ABCD", "gitlab fan": "U0777ABCD"})
	if err != nil {
		t.Fatalf("host-prefixed plain names must load: %v", err)
	}
	c2 := Config{AuthorIDs: got2}
	if c2.AuthorID("Github Chen") != "U0123ABCD" {
		t.Fatal("host-prefixed plain names bind as names")
	}
	// A malformed qualified account key (no colon) is still rejected.
	if _, err := normalizeAuthorIDs(map[string]string{"gitlab@dops": "U0123ABCD"}); err == nil {
		t.Fatal("a qualified key without a colon must be rejected")
	}
}

func TestLLMIsOptional(t *testing.T) {
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "missing-config.yaml"))
	setMinimalValidConfigEnv(t)
	t.Setenv("LLM_PROVIDER", "")
	t.Setenv("OPENAI_API_KEY", "")
	cfg := LoadConfig()
	if cfg.LLMConfigured() || cfg.SimilarItemCount != 30 {
		t.Fatalf("no LLM configured and the default similar-item count: %+v", cfg)
	}
}

func TestAccountAuthorIDByUsername(t *testing.T) {
	got, err := normalizeAuthorIDs(map[string]string{"gitlab:YFMA": "UYFMA0001", "github:octo-cat": "UOCTO0001",
		"gitlab:24": "UID000024", "gitlab:dlee": "UDLEE0001", "github:12": "UGH0000012"})
	if err != nil {
		t.Fatal(err)
	}
	c := Config{AuthorIDs: got, GitLabSources: []GitLabSource{{Name: "dops", Default: true}}}
	cases := []struct {
		host, server string
		id           int64
		username     string
		want         string
		warn         bool
	}{
		{"gitlab", "", 99, "yfma", "UYFMA0001", false},    // default-source username key, case-insensitive
		{"gitlab", "", 0, "YfMa", "UYFMA0001", false},     // no numeric ID needed
		{"github", "", 5, "Octo-Cat", "UOCTO0001", false}, // github login
		{"github", "", 5, "yfma", "", false},              // username keys are per host
		{"gitlab", "", 24, "yfma", "UID000024", true},     // ID key wins over a conflicting username key
		{"gitlab", "", 24, "nobody", "UID000024", false},  // ID key alone
		{"gitlab", "dops", 24, "yfma", "UID000024", true}, // the default server resolves unqualified too
		{"gitlab", "corp", 24, "yfma", "", false},         // a non-default server never sees unqualified keys
		{"gitlab", "", 99, "yfm", "", false},              // never a prefix or fuzzy match
		{"gitlab", "", 99, "Dana Lee", "", false},         // display names are not usernames
		{"github", "", 99, "12", "", false},               // an all-digit login never reaches an ID key
	}
	for _, tc := range cases {
		id, warn := c.AccountAuthorID(tc.host, tc.server, tc.id, tc.username)
		if id != tc.want || (warn != "") != tc.warn {
			t.Errorf("%s:%d/%q: got %q warn %q, want %q warn %v", tc.host, tc.id, tc.username, id, warn, tc.want, tc.warn)
		}
	}
	_, warn := c.AccountAuthorID("gitlab", "", 24, "yfma")
	if !strings.Contains(warn, `"gitlab:24"`) || !strings.Contains(warn, `"gitlab:yfma"`) {
		t.Fatalf("warning must name both keys: %q", warn)
	}
	c.AuthorIDs["gitlab:yfma"] = "UID000024"
	if _, warn := c.AccountAuthorID("gitlab", "", 24, "yfma"); warn != "" {
		t.Fatalf("agreeing keys are no conflict: %q", warn)
	}
}

func TestGitLabSourcesValidation(t *testing.T) {
	good := GitLabSource{Name: "dops", URL: "https://dops.example", Token: "t", GroupID: "g", Default: true}
	if err := validateGitLabSource(good); err != nil {
		t.Fatalf("group source must validate: %v", err)
	}
	commits := GitLabSource{Name: "corp", URL: "https://corp.example", Token: "t", Project: "my-group/my-project", Branch: "br_8-0"}
	if err := validateGitLabSource(commits); err != nil {
		t.Fatalf("commit source must validate: %v", err)
	}
	for _, bad := range []GitLabSource{
		{Name: "", URL: "https://x", Token: "t", GroupID: "g"},
		{Name: "Dops", URL: "https://x", Token: "t", GroupID: "g"},     // uppercase
		{Name: "dops:bad", URL: "https://x", Token: "t", GroupID: "g"}, // colon in name
		{Name: "dops", URL: "", Token: "t", GroupID: "g"},
		{Name: "dops", URL: "https://x", Token: "", GroupID: "g"},
		{Name: "dops", URL: "https://x", Token: "t"},                            // neither group nor branch
		{Name: "dops", URL: "https://x", Token: "t", GroupID: "g", Branch: "b"}, // both
		// An empty ref_name would silently fetch the project's default branch.
		{Name: "dops", URL: "https://x", Token: "t", Project: "my-group/my-project"},
		{Name: "dops", URL: "https://x", Token: "t", Branch: "br_8-0"},
	} {
		if err := validateGitLabSource(bad); err == nil {
			t.Fatalf("%+v must be rejected", bad)
		}
	}
	if err := validateGitLabSourceNames([]GitLabSource{good, commits}); err != nil {
		t.Fatalf("one default among two sources must validate: %v", err)
	}
	if err := validateGitLabSourceNames([]GitLabSource{{Name: "a", URL: "x", Token: "t", GroupID: "g"}, {Name: "b", URL: "x", Token: "t", GroupID: "g"}}); err == nil {
		t.Fatal("no default source must be rejected")
	}
	two := good
	two.Name = "corp"
	if err := validateGitLabSourceNames([]GitLabSource{good, two}); err == nil {
		t.Fatal("two defaults must be rejected")
	}
	dup := good
	dup.Default = false
	dup2 := dup
	if err := validateGitLabSourceNames([]GitLabSource{dup, dup2}); err == nil {
		t.Fatal("duplicate names must be rejected")
	}
}

func TestLoadGitLabSourcesFromYAMLWithEnvToken(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := `
gitlab_sources:
  - name: dops
    url: "https://dops.example"
    token: ${DOPS_GITLAB_TOKEN}
    group_id: "my-group"
    ref_ticket_label: "Mantis"
    default: true
  - name: corp
    url: "https://corp.example"
    token: "plain-token"
    project: "my-group/my-project"
    branch: "br_8-0_feature"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", cfgPath)
	t.Setenv("DOPS_GITLAB_TOKEN", "glpat-env")
	setMinimalValidConfigEnv(t)

	cfg := LoadConfig()
	if len(cfg.GitLabSources) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(cfg.GitLabSources))
	}
	dops, corp := cfg.GitLabSources[0], cfg.GitLabSources[1]
	if dops.Token != "glpat-env" {
		t.Fatalf("${DOPS_GITLAB_TOKEN} must interpolate, got %q", dops.Token)
	}
	if corp.Token != "plain-token" {
		t.Fatalf("plain token stays, got %q", corp.Token)
	}
	if !dops.Default || corp.Default {
		t.Fatalf("default flags: %+v %+v", dops, corp)
	}
	if dops.GroupID != "my-group" || dops.RefTicketLabel != "Mantis" {
		t.Fatalf("group source fields: %+v", dops)
	}
	if corp.Project != "my-group/my-project" || corp.Branch != "br_8-0_feature" {
		t.Fatalf("commit source fields: %+v", corp)
	}
	// A server-qualified key binds only its server; the unqualified form only the default.
	cfg.AuthorIDs = map[string]string{
		"gitlab@corp:yfma": "UCORP00001", "gitlab:yfma": "UDOPS00001", "gitlab@dops:24": "UDOPS00024",
	}
	if id, _ := cfg.AccountAuthorID("gitlab", "corp", 0, "YFMA"); id != "UCORP00001" {
		t.Fatalf("qualified corp key: got %q", id)
	}
	if id, _ := cfg.AccountAuthorID("gitlab", "dops", 0, "yfma"); id != "UDOPS00001" {
		t.Fatalf("default server sees the unqualified key: got %q", id)
	}
	if id, _ := cfg.AccountAuthorID("gitlab", "corp", 24, "yfma"); id != "UCORP00001" {
		t.Fatalf("corp binds its qualified username key: got %q", id)
	}
	if id, _ := cfg.AccountAuthorID("gitlab", "corp", 24, ""); id != "" {
		t.Fatalf("corp must not see dops' unqualified ID key: got %q", id)
	}
	if id, _ := cfg.AccountAuthorID("gitlab", "dops", 24, ""); id != "UDOPS00024" {
		t.Fatalf("qualified dops ID key: got %q", id)
	}
}

func TestLoadConfigFatalOnLegacyGitLabKeys(t *testing.T) {
	if os.Getenv("TEST_LEGACY_GITLAB_FATAL") == "1" {
		_ = os.Setenv("CONFIG_PATH", filepath.Join(os.TempDir(), "legacy-config.yaml"))
		content := "gitlab_url: https://gitlab.example.com\ngitlab_token: t\ngitlab_group_id: g\n"
		_ = os.WriteFile(os.Getenv("CONFIG_PATH"), []byte(content), 0o644)
		LoadConfig()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestLoadConfigFatalOnLegacyGitLabKeys")
	cmd.Env = append(os.Environ(), "TEST_LEGACY_GITLAB_FATAL=1",
		"SLACK_BOT_TOKEN=xoxb-test", "SLACK_APP_TOKEN=xapp-test",
		"DECISION_MODEL_URL=https://api.compat.example", "DECISION_MODEL_API_KEY=dm-test", "DECISION_MODEL_NAME=test-model",
		"TIMEZONE=UTC")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected subprocess to exit with failure, output: %s", out)
	}
	if !strings.Contains(string(out), "gitlab_sources") || !strings.Contains(string(out), "gitlab_url") {
		t.Fatalf("fatal error must name the replacement: %s", out)
	}
}

// A token placeholder whose env var is not set must fail startup, not fetch time.
func TestLoadConfigFatalOnMissingEnvToken(t *testing.T) {
	if os.Getenv("TEST_MISSING_ENV_TOKEN_FATAL") == "1" {
		_ = os.Setenv("CONFIG_PATH", filepath.Join(os.TempDir(), "missing-env-token.yaml"))
		content := "gitlab_sources:\n  - name: dops\n    url: https://gitlab.example.com\n    token: ${UNSET_TOKEN}\n    group_id: g\n    default: true\n"
		_ = os.WriteFile(os.Getenv("CONFIG_PATH"), []byte(content), 0o644)
		LoadConfig()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestLoadConfigFatalOnMissingEnvToken")
	cmd.Env = append(os.Environ(), "TEST_MISSING_ENV_TOKEN_FATAL=1",
		"SLACK_BOT_TOKEN=xoxb-test", "SLACK_APP_TOKEN=xapp-test",
		"DECISION_MODEL_URL=https://api.compat.example", "DECISION_MODEL_API_KEY=dm-test", "DECISION_MODEL_NAME=test-model",
		"TIMEZONE=UTC")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected subprocess to exit with failure, output: %s", out)
	}
	if !strings.Contains(string(out), "token is required") {
		t.Fatalf("fatal error must name the missing token: %s", out)
	}
}
