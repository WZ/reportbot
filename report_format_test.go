package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteReportAndEmailDraftFiles(t *testing.T) {
	outDir := t.TempDir()
	date := time.Date(2026, 2, 20, 0, 0, 0, 0, time.UTC)

	reportPath, err := WriteReportFile("hello report\n", outDir, date, "Team A")
	if err != nil {
		t.Fatalf("WriteReportFile failed: %v", err)
	}
	if !strings.HasSuffix(reportPath, "Team A_20260220.md") {
		t.Fatalf("unexpected report file path: %s", reportPath)
	}
	if data, err := os.ReadFile(reportPath); err != nil || string(data) != "hello report\n" {
		t.Fatalf("unexpected report file content err=%v content=%q", err, string(data))
	}

	emlPath, err := WriteEmailDraftFile("email body", outDir, date, "Team A")
	if err != nil {
		t.Fatalf("WriteEmailDraftFile failed: %v", err)
	}
	if !strings.HasSuffix(emlPath, "Team A_20260220.eml") {
		t.Fatalf("unexpected eml file path: %s", emlPath)
	}
	if _, err := os.Stat(filepath.Clean(emlPath)); err != nil {
		t.Fatalf("expected eml file to exist: %v", err)
	}
}

func TestWriteReportFileSanitizesTeamName(t *testing.T) {
	outDir := t.TempDir()
	date := time.Date(2026, 2, 20, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		team         string
		expectSuffix string
	}{
		{
			name:         "path separators only",
			team:         "../Ops\\Team",
			expectSuffix: "_Ops_Team_20260220.md",
		},
		{
			name:         "path traversal with special characters",
			team:         "../../Team:Name<>|*?",
			expectSuffix: "",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			reportPath, err := WriteReportFile("hello report\n", outDir, date, tc.team)
			if err != nil {
				t.Fatalf("WriteReportFile failed: %v", err)
			}
			if tc.expectSuffix != "" {
				if !strings.HasSuffix(reportPath, tc.expectSuffix) {
					t.Fatalf("unexpected sanitized report file path: %s", reportPath)
				}
			} else {
				base := filepath.Base(reportPath)
				if strings.ContainsAny(base, `/\:*?"<>|`) {
					t.Fatalf("sanitized report filename contains invalid characters: %q", base)
				}
			}
			if _, err := os.Stat(filepath.Clean(reportPath)); err != nil {
				t.Fatalf("expected report file to exist: %v", err)
			}

			rel, err := filepath.Rel(filepath.Clean(outDir), filepath.Clean(reportPath))
			if err != nil {
				t.Fatalf("failed to compute relative path: %v", err)
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				t.Fatalf("report path escaped output directory: %s", reportPath)
			}
			if strings.Contains(rel, string(os.PathSeparator)) {
				t.Fatalf("sanitized report filename unexpectedly contains path separators: %s", rel)
			}
		})
	}
}

func TestBuildEMLAndMarkdownTransforms(t *testing.T) {
	body := "### Title\n\n- **Alice** - item one (done)\n- item two (in progress)\n"
	eml := buildEML("Weekly Subject", body)

	if !strings.Contains(eml, "Subject: Weekly Subject") {
		t.Fatalf("expected subject in eml, got:\n%s", eml)
	}
	if !strings.Contains(eml, "Content-Type: multipart/alternative") {
		t.Fatalf("expected multipart header in eml")
	}
	if !strings.Contains(eml, "Content-Type: text/plain; charset=UTF-8") {
		t.Fatalf("expected plain text part in eml")
	}
	if !strings.Contains(eml, "Content-Type: text/html; charset=UTF-8") {
		t.Fatalf("expected html part in eml")
	}

	plain := markdownToEmailPlain(body)
	if strings.Contains(plain, "**") {
		t.Fatalf("plain output should strip markdown bold markers: %q", plain)
	}
	if !strings.Contains(plain, "Alice - item one (done)") {
		t.Fatalf("unexpected plain conversion: %q", plain)
	}

	html := markdownToEmailHTML(body)
	if !strings.Contains(html, "<strong>Alice</strong>") {
		t.Fatalf("expected bold author in html output: %s", html)
	}
	if !strings.Contains(html, "<ul") || !strings.Contains(html, "<li") {
		t.Fatalf("expected list tags in html output: %s", html)
	}
}

func TestBuildEMLSanitizesInjectedSubjectHeaders(t *testing.T) {
	eml := buildEML("Weekly Report\r\nBcc: attacker@example.com", "body")

	parts := strings.SplitN(eml, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatalf("invalid eml format, missing header/body boundary: %q", eml)
	}
	headers := parts[0]

	if strings.Contains(headers, "\r\nBcc: attacker@example.com") {
		t.Fatalf("subject header injection not sanitized: %q", headers)
	}
	if !strings.Contains(headers, "Subject: Weekly Report Bcc: attacker@example.com") {
		t.Fatalf("expected sanitized subject line in headers: %q", headers)
	}
}

func TestReportHelpers(t *testing.T) {
	if got := sanitizeFilename(`a/b\c:d*e?f"g<h>i|j`); strings.ContainsAny(got, `/\\:*?"<>|`) {
		t.Fatalf("sanitizeFilename left invalid characters: %q", got)
	}

	crlf := normalizeCRLF("a\nb\r\nc\n")
	if strings.Count(crlf, "\r\n") != 3 {
		t.Fatalf("normalizeCRLF did not normalize newlines: %q", crlf)
	}

	html := bodyToHTML("line1\nline2")
	if !strings.Contains(html, "line1<br>") || !strings.Contains(html, "line2") {
		t.Fatalf("bodyToHTML unexpected output: %s", html)
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"basic path chars", `a/b\c:d*e?f"g<h>i|j`, "a_b_c_d_e_f_g_h_i_j"},
		{"null byte", "team\x00name", "teamname"},
		{"control chars", "team\x01\x02\x1Fname", "teamname"},
		{"delete char", "team\x7Fname", "teamname"},
		{"empty after sanitization", "/\\/\\", "report"},
		{"all dots", "...", "report"},
		{"leading dots", "...team", "team"},
		{"trailing dots", "team...", "team"},
		{"leading spaces", "  team", "team"},
		{"trailing spaces", "team  ", "team"},
		{"dots and spaces", " ... ", "report"},
		{"mixed control and path", "te\x00am/na\x1Fme", "team_name"},
		{"normal name", "Team A", "Team A"},
		{"single dot", ".", "report"},
		{"double dot", "..", "report"},
		{"empty string", "", "report"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeFilename(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
