package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

var dayMap = map[string]time.Weekday{
	"sunday":    time.Sunday,
	"monday":    time.Monday,
	"tuesday":   time.Tuesday,
	"wednesday": time.Wednesday,
	"thursday":  time.Thursday,
	"friday":    time.Friday,
	"saturday":  time.Saturday,
}

func StartNudgeScheduler(cfg Config, api *slack.Client) {
	if len(cfg.TeamMembers) == 0 {
		log.Println("No team_members configured, nudge disabled")
		return
	}

	memberIDs, unresolved, err := resolveUserIDs(api, cfg.TeamMembers)
	if err != nil {
		log.Printf("Error resolving team_members: %v", err)
		if len(memberIDs) == 0 {
			return
		}
	}
	if len(unresolved) > 0 {
		log.Printf("Unresolved team_members: %s", strings.Join(unresolved, ", "))
	}

	weekday, ok := dayMap[strings.ToLower(cfg.NudgeDay)]
	if !ok {
		log.Printf("Invalid nudge_day '%s', using Friday", cfg.NudgeDay)
		weekday = time.Friday
	}

	hour, min, err := parseTime(cfg.NudgeTime)
	if err != nil {
		log.Printf("Invalid nudge_time '%s': %v, using 10:00", cfg.NudgeTime, err)
		hour, min = 10, 0
	}

	log.Printf("Nudge scheduled every %s at %02d:%02d for %d team members", weekday, hour, min, len(cfg.TeamMembers))

	go func() {
		for {
			now := time.Now()
			next := nextWeekday(now, weekday, hour, min)
			wait := next.Sub(now)
			log.Printf("Next nudge at %s (in %s)", next.Format("Mon Jan 2 15:04"), wait.Round(time.Minute))

			time.Sleep(wait)
			sendNudges(api, cfg, memberIDs, cfg.ReportChannelID)
		}
	}()
}

func nextWeekday(now time.Time, day time.Weekday, hour, min int) time.Time {
	daysUntil := (day - now.Weekday() + 7) % 7
	if daysUntil == 0 {
		target := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
		if now.Before(target) {
			return target
		}
		daysUntil = 7
	}
	return time.Date(now.Year(), now.Month(), now.Day()+int(daysUntil), hour, min, 0, 0, now.Location())
}

func sendNudges(api *slack.Client, cfg Config, memberIDs []string, reportChannelID string) {
	monday, nextMonday := ReportWeekRange(cfg, time.Now())
	channelRef := ""
	if reportChannelID != "" {
		channelRef = fmt.Sprintf(" Please report in <#%s>.", reportChannelID)
	}
	msg := fmt.Sprintf(
		"Hey! Friendly reminder to report your work items for this week (%s - %s) using `/report`.%s\n"+
			"Example: `/report [mantis_id] Add pagination to user list API (done)`",
		monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2"),
		channelRef,
	)

	for _, userID := range memberIDs {
		channel, _, _, err := api.OpenConversation(&slack.OpenConversationParameters{
			Users: []string{userID},
		})
		if err != nil {
			log.Printf("Error opening DM with %s: %v", userID, err)
			continue
		}

		_, _, err = api.PostMessage(channel.ID, slack.MsgOptionText(msg, false))
		if err != nil {
			log.Printf("Error sending nudge to %s: %v", userID, err)
		} else {
			log.Printf("Sent nudge to %s", userID)
		}
	}
}

func parseTime(s string) (int, int, error) {
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
