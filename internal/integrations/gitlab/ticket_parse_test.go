package gitlab

import "testing"

func TestParseTicketIDsFromDescription(t *testing.T) {
	tests := []struct {
		name        string
		fieldLabel  string
		description string
		want        string
	}{
		{
			name:        "single hash ticket",
			fieldLabel:  "Jira",
			description: "Jira: #7001001",
			want:        "7001001",
		},
		{
			name:        "single prefixed ticket",
			fieldLabel:  "Jira",
			description: "Jira: JIRA-7001002",
			want:        "7001002",
		},
		{
			name:        "comma separated mixed formats",
			fieldLabel:  "Jira",
			description: "Jira: #7001003, JIRA-7001004",
			want:        "7001003,7001004",
		},
		{
			name:       "heading and value on next line",
			fieldLabel: "Jira",
			description: `## Purpose:
[7001005] Improve Atlas sync
## Jira:
#7001005

## Description:
Adjust retries for Atlas sync`,
			want: "7001005",
		},
		{
			name:        "empty field means no tickets",
			fieldLabel:  "Jira",
			description: "Jira:",
			want:        "",
		},
		{
			name:        "missing field means no tickets",
			fieldLabel:  "Jira",
			description: "Purpose: tighten health check timeout",
			want:        "",
		},
		{
			name:        "empty label disables parsing",
			fieldLabel:  "",
			description: "Jira: #7001010",
			want:        "",
		},
		{
			name:       "custom field label",
			fieldLabel: "Tracker",
			description: `## Tracker:
TRACKER-7001006, #7001007`,
			want: "7001006,7001007",
		},
		{
			name:        "dedupe while preserving order",
			fieldLabel:  "Jira",
			description: "Jira: #7001008, JIRA-7001008, #7001009",
			want:        "7001008,7001009",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTicketIDsFromDescription(tt.description, tt.fieldLabel)
			if got != tt.want {
				t.Fatalf("parseTicketIDsFromDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}
