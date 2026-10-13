package slackbot

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

func resetTeamNamesCache(t *testing.T) {
	t.Helper()
	teamNamesCache.Lock()
	teamNamesCache.byID = nil
	teamNamesCache.Unlock()
	t.Cleanup(func() {
		teamNamesCache.Lock()
		teamNamesCache.byID = nil
		teamNamesCache.Unlock()
	})
}

func TestTeamProfileNamesResolvesAndCaches(t *testing.T) {
	resetTeamNamesCache(t)
	calls := 0
	getUser := func(id string) (*slack.User, error) {
		calls++
		u := user(id, "Grant Guo", "grantg")
		return &u, nil
	}
	names, err := TeamProfileNames(getUser)([]string{"UGRANT0001"})
	if err != nil || !reflect.DeepEqual(names, []string{"Grant Guo", "grantg"}) {
		t.Fatalf("names = %v, err = %v", names, err)
	}
	if _, err := TeamProfileNames(getUser)([]string{"UGRANT0001"}); err != nil || calls != 1 {
		t.Fatalf("second lookup within the TTL should be cached: calls=%d err=%v", calls, err)
	}

	teamNamesCache.Lock()
	e := teamNamesCache.byID["UGRANT0001"]
	e.at = time.Now().Add(-teamNamesTTL - time.Minute)
	teamNamesCache.byID["UGRANT0001"] = e
	teamNamesCache.Unlock()
	if _, err := TeamProfileNames(getUser)([]string{"UGRANT0001"}); err != nil || calls != 2 {
		t.Fatalf("expired entry should be looked up again: calls=%d err=%v", calls, err)
	}
}

func TestTeamProfileNamesPartialFailureIsNotCached(t *testing.T) {
	resetTeamNamesCache(t)
	fail := true
	getUser := func(id string) (*slack.User, error) {
		if id == "UBROKEN01" && fail {
			return nil, errors.New("user_not_found")
		}
		u := user(id, "Name "+id, "")
		return &u, nil
	}
	names, err := TeamProfileNames(getUser)([]string{"UGOOD0001", "UBROKEN01"})
	if err == nil || !strings.Contains(err.Error(), "UBROKEN01") {
		t.Fatalf("expected an error naming the failed ID, got %v", err)
	}
	if !reflect.DeepEqual(names, []string{"Name UGOOD0001"}) {
		t.Fatalf("resolved names should still be returned, got %v", names)
	}
	fail = false
	names, err = TeamProfileNames(getUser)([]string{"UGOOD0001", "UBROKEN01"})
	if err != nil || len(names) != 2 {
		t.Fatalf("failed lookup should be retried: names=%v err=%v", names, err)
	}
}
