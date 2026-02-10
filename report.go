package main

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func WriteReportFile(content, outputDir string, reportDate time.Time, teamName string) (string, error) {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return "", err
	}
	filename := fmt.Sprintf("%s_%s.md", teamName, reportDate.Format("20060102"))
	path := filepath.Join(outputDir, filename)
	return path, os.WriteFile(path, []byte(content), 0644)
}

func WriteEmailDraftFile(body, outputDir string, reportDate time.Time, subjectPrefix string) (string, error) {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return "", err
	}
	filename := fmt.Sprintf("%s_%s.eml", sanitizeFilename(subjectPrefix), reportDate.Format("20060102"))
	path := filepath.Join(outputDir, filename)
	subject := fmt.Sprintf("%s %s", subjectPrefix, reportDate.Format("20060102"))
	content := buildEML(subject, body)
	return path, os.WriteFile(path, []byte(content), 0644)
}

func buildEML(subject, body string) string {
	const boundary = "reportbot-alt"
	headers := []string{
		"MIME-Version: 1.0",
		fmt.Sprintf("Content-Type: multipart/alternative; boundary=%q", boundary),
		fmt.Sprintf("Subject: %s", subject),
	}
	plain := normalizeCRLF(markdownToEmailPlain(body))
	htmlBody := markdownToEmailHTML(body)

	var out strings.Builder
	out.WriteString(strings.Join(headers, "\r\n"))
	out.WriteString("\r\n\r\n")
	out.WriteString("--" + boundary + "\r\n")
	out.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	out.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	out.WriteString(plain)
	if !strings.HasSuffix(plain, "\r\n") {
		out.WriteString("\r\n")
	}
	out.WriteString("\r\n--" + boundary + "\r\n")
	out.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	out.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	out.WriteString(htmlBody)
	out.WriteString("\r\n--" + boundary + "--\r\n")
	return out.String()
}

func sanitizeFilename(s string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	return replacer.Replace(s)
}

func normalizeCRLF(s string) string {
	normalized := strings.ReplaceAll(s, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\n", "\r\n")
	return normalized
}

func bodyToHTML(body string) string {
	escaped := html.EscapeString(strings.ReplaceAll(body, "\r\n", "\n"))
	escaped = strings.ReplaceAll(escaped, "\n", "<br>\n")
	return `<html><body style="font-family: Calibri, Arial, sans-serif; font-size: 11pt; color: #1f1f1f; line-height: 1.35;">` + escaped + `</body></html>`
}

func markdownToEmailPlain(body string) string {
	var out []string
	prevBlank := false
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "### ") || strings.HasPrefix(trimmed, "#### ") {
			line = strings.TrimSpace(strings.TrimLeft(trimmed, "# "))
		}
		line = strings.ReplaceAll(line, "**", "")
		if strings.TrimSpace(line) == "" {
			if prevBlank {
				continue
			}
			prevBlank = true
			out = append(out, "")
			continue
		}
		prevBlank = false
		out = append(out, line)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}

var boldTokenRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)

func markdownToEmailHTML(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	var b strings.Builder
	b.WriteString(`<html><body style="font-family: Calibri, Arial, sans-serif; font-size: 11pt; color: #1f1f1f; line-height: 1.35;">`)
	levelHasOpenLI := make([]bool, 0, 4)
	currentLevel := -1

	closeToLevel := func(target int) {
		for currentLevel > target {
			if currentLevel < len(levelHasOpenLI) && levelHasOpenLI[currentLevel] {
				b.WriteString(`</li>`)
				levelHasOpenLI[currentLevel] = false
			}
			b.WriteString(`</ul>`)
			levelHasOpenLI = levelHasOpenLI[:len(levelHasOpenLI)-1]
			currentLevel--
		}
	}

	closeAllLists := func() {
		closeToLevel(0)
		if currentLevel == 0 {
			if levelHasOpenLI[0] {
				b.WriteString(`</li>`)
				levelHasOpenLI[0] = false
			}
			b.WriteString(`</ul>`)
			levelHasOpenLI = levelHasOpenLI[:0]
			currentLevel = -1
		}
	}

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			closeAllLists()
			b.WriteString(`<div style="height: 10px;"></div>`)
			continue
		}

		if strings.HasPrefix(trimmed, "### ") || strings.HasPrefix(trimmed, "#### ") {
			closeAllLists()
			text := renderInlineBold(strings.TrimSpace(strings.TrimLeft(trimmed, "# ")))
			b.WriteString(`<div style="font-weight: 700; margin: 12px 0 6px 0;">` + text + `</div>`)
			continue
		}

		leading := len(line) - len(strings.TrimLeft(line, " "))
		content := strings.TrimLeft(line, " ")
		if strings.HasPrefix(content, "- ") {
			textRaw := strings.TrimSpace(strings.TrimPrefix(content, "- "))
			text := renderInlineBold(textRaw)
			level := leading / 2
			if level < 0 {
				level = 0
			}

			if currentLevel == -1 {
				for i := 0; i <= level; i++ {
					b.WriteString(`<ul style="margin: 0 0 0 18px; padding-left: 18px; list-style-type: disc;">`)
					levelHasOpenLI = append(levelHasOpenLI, false)
					currentLevel++
				}
			} else if level > currentLevel {
				for i := currentLevel + 1; i <= level; i++ {
					b.WriteString(`<ul style="margin: 0 0 0 18px; padding-left: 18px; list-style-type: disc;">`)
					levelHasOpenLI = append(levelHasOpenLI, false)
					currentLevel++
				}
			} else if level < currentLevel {
				closeToLevel(level)
			}

			if currentLevel >= 0 && levelHasOpenLI[currentLevel] {
				b.WriteString(`</li>`)
				levelHasOpenLI[currentLevel] = false
			}
			b.WriteString(`<li style="margin: 2px 0;">` + text)
			levelHasOpenLI[currentLevel] = true
			continue
		}

		closeAllLists()
		text := renderInlineBold(strings.TrimSpace(content))
		b.WriteString(`<div style="margin: 2px 0;">` + text + `</div>`)
	}
	closeAllLists()
	b.WriteString(`</body></html>`)
	return b.String()
}

func renderInlineBold(s string) string {
	matches := boldTokenRe.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return html.EscapeString(s)
	}
	var out strings.Builder
	last := 0
	for _, m := range matches {
		if len(m) < 4 {
			continue
		}
		out.WriteString(html.EscapeString(s[last:m[0]]))
		out.WriteString("<strong>")
		out.WriteString(html.EscapeString(s[m[2]:m[3]]))
		out.WriteString("</strong>")
		last = m[1]
	}
	out.WriteString(html.EscapeString(s[last:]))
	return out.String()
}
