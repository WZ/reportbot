package filing

import (
	"reflect"
	"testing"

	"reportbot/internal/domain"
)

func TestSplitAndMapStatus(t *testing.T) {
	d, st := SplitStatus("[123] Fix it (root cause analysis in progress)")
	if d != "[123] Fix it" || st != "root cause analysis in progress" {
		t.Fatalf("got %q %q", d, st)
	}
	if d, st := SplitStatus("No status here"); d != "No status here" || st != "" {
		t.Fatalf("got %q %q", d, st)
	}
	for text, want := range map[string]string{"Done": domain.StatusDone, " in  testing ": domain.StatusInTesting,
		"WIP": domain.StatusInProgress, "blocked": domain.StatusBlocked, "on hold": domain.StatusBlocked} {
		if got, ok := domain.StatusFromWord(text); !ok || got != want {
			t.Errorf("StatusFromWord(%q) = %q,%v", text, got, ok)
		}
	}
	if _, ok := domain.StatusFromWord("root cause analysis in progress"); ok {
		t.Error("free text must go to the model")
	}
}

func TestFindTickets(t *testing.T) {
	cases := []struct {
		in        string
		certain   []string
		candidate []string
	}{
		{"[1234567, 7654321] fix", []string{"1234567", "7654321"}, nil},
		{"fix crash 1234567 on upgrade", []string{"1234567"}, nil},
		{"7.6.3 build 1459 verified", nil, nil},
		{"backport 1234567 after MR !98765", []string{"1234567"}, nil},
		{"build 1234567 nightly", nil, nil},
		{"crash 1234567 also seen as 7654321", nil, []string{"1234567", "7654321"}},
		{"no numbers at all", nil, nil},
		{"upgrade to 7.6.3", nil, nil},
	}
	for _, c := range cases {
		certain, cands := FindTickets(c.in)
		var nums []string
		for _, x := range cands {
			nums = append(nums, x.Number)
		}
		if !reflect.DeepEqual(certain, c.certain) || !reflect.DeepEqual(nums, c.candidate) {
			t.Errorf("FindTickets(%q) = %v %v, want %v %v", c.in, certain, nums, c.certain, c.candidate)
		}
	}
}

func TestBuildRequestOnlyAsksWhatCodeCannotDecide(t *testing.T) {
	_, qs := BuildRequest(RequestInput{Item: domain.WorkItem{Description: "x"}})
	if len(qs) != 0 {
		t.Fatalf("expected no questions, got %v", qs)
	}
	state, qs := BuildRequest(RequestInput{Item: domain.WorkItem{Description: "x"}, AskStatus: true,
		Tickets: []TicketCandidate{{Number: "1459", Context: "build 1459"}}})
	if _, ok := qs[QStatus]; !ok || state["status_text"] != "" {
		t.Fatalf("expected a status question with empty status_text: %v %v", qs, state)
	}
	crit := qs[QTicket].Criteria.(map[string]string)
	if crit["t1"] != "`ticket_candidates.t1`" || crit["none"] == "" {
		t.Fatalf("unexpected ticket criteria: %v", crit)
	}
	if TicketFromChoice("t1", []TicketCandidate{{Number: "1459"}}) != "1459" || TicketFromChoice("none", nil) != "" {
		t.Fatal("unexpected ticket mapping")
	}
}
