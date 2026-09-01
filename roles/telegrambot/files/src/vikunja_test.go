package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A project carries four views. Asking the list one is what makes "top task"
// mean the task at the top of the list in the interface.
func TestListViewIsTheOneAsked(t *testing.T) {
	p := vikunjaProject{Views: []vikunjaView{
		{ID: 9, Kind: "gantt"},
		{ID: 5, Kind: "list"},
		{ID: 7, Kind: "kanban"},
	}}
	id, ok := p.listViewID()
	if !ok || id != 5 {
		t.Errorf("listViewID = %d, %v; want 5, true", id, ok)
	}
	if _, ok := (vikunjaProject{}).listViewID(); ok {
		t.Error("a project with no views reported one")
	}
}

// Vikunja sends a year-1 timestamp rather than null for a task with no due
// date. Treated as a real date it parses fine and simply never matches, but
// the intent is that it is absent.
func TestNoDueDateIsNotADate(t *testing.T) {
	day := time.Date(2026, 9, 1, 12, 0, 0, 0, time.Local)
	for _, v := range []string{"", vikunjaNoDueDate} {
		if (vikunjaTask{DueDate: v}).dueOn(day) {
			t.Errorf("task with due_date %q counted as due", v)
		}
	}
	if (vikunjaTask{DueDate: "not a date"}).dueOn(day) {
		t.Error("an unparseable due date counted as due")
	}
}

// The comparison is by calendar day in the day's own location, so a task due
// late in the evening is due today and one due just after midnight is not.
func TestDueOnComparesCalendarDays(t *testing.T) {
	day := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		due  string
		want bool
	}{
		{"2026-09-01T23:59:00Z", true},
		{"2026-09-01T00:00:01Z", true},
		{"2026-09-02T00:00:01Z", false},
		{"2026-08-31T23:59:00Z", false},
	}
	for _, c := range cases {
		if got := (vikunjaTask{DueDate: c.due}).dueOn(day); got != c.want {
			t.Errorf("dueOn(%s) = %v, want %v", c.due, got, c.want)
		}
	}
}

// Serves the shape the real API returns: projects carrying their views, and a
// per-view task list.
func vikunjaStub(t *testing.T, tasks map[int64][]vikunjaTask) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer testtoken" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/projects":
			json.NewEncoder(w).Encode([]vikunjaProject{
				{ID: 1, Title: "Inbox", Views: []vikunjaView{{ID: 1, Kind: "list"}}},
				{ID: 2, Title: "Dining Room", Views: []vikunjaView{{ID: 5, Kind: "list"}}},
				{ID: 3, Title: "Archived", Archived: true, Views: []vikunjaView{{ID: 9, Kind: "list"}}},
			})
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			var view int64
			fmt.Sscanf(r.URL.Path, "/api/v1/projects/%d/views/%d/tasks", new(int64), &view)
			json.NewEncoder(w).Encode(tasks[view])
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestNextTaskTakesTheFirstOfEachProject(t *testing.T) {
	srv := vikunjaStub(t, map[int64][]vikunjaTask{
		1: {{Title: "First in Inbox"}, {Title: "Second in Inbox"}},
		5: {{Title: "First in Dining"}, {Title: "Second in Dining"}},
		9: {{Title: "In an archived project"}},
	})
	defer srv.Close()

	b := &Bot{VikunjaURL: srv.URL, VikunjaToken: "testtoken"}
	out := b.nextTask(context.Background())

	for _, want := range []string{"First in Inbox", "First in Dining"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"Second in Inbox", "Second in Dining", "In an archived project"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output should not contain %q:\n%s", unwanted, out)
		}
	}
}

// A project whose tasks are all done contributes nothing rather than an empty
// bullet.
func TestNextTaskSkipsProjectsWithNothingOpen(t *testing.T) {
	srv := vikunjaStub(t, map[int64][]vikunjaTask{
		1: {{Title: "Still open"}},
		5: {{Title: "Finished", Done: true}},
	})
	defer srv.Close()

	out := (&Bot{VikunjaURL: srv.URL, VikunjaToken: "testtoken"}).nextTask(context.Background())
	if strings.Contains(out, "Dining Room") {
		t.Errorf("a project with nothing open was listed:\n%s", out)
	}
	if !strings.Contains(out, "Still open") {
		t.Errorf("output missing the open task:\n%s", out)
	}
}

func TestNextTaskListsWhatIsDueToday(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	srv := vikunjaStub(t, map[int64][]vikunjaTask{
		1: {
			{Title: "No due date"},
			{Title: "Due today", DueDate: today + "T15:00:00Z"},
		},
		5: {{Title: "Due next year", DueDate: "2099-01-01T09:00:00Z"}},
	})
	defer srv.Close()

	out := (&Bot{VikunjaURL: srv.URL, VikunjaToken: "testtoken"}).nextTask(context.Background())

	// Only the section below the heading. A task can legitimately appear above
	// it as its project's top task, which is a different claim.
	_, dueSection, found := strings.Cut(out, "Due today</b>")
	if !found {
		t.Fatalf("no due today section:\n%s", out)
	}
	if !strings.Contains(dueSection, "Due today") {
		t.Errorf("the task due today is missing from the due section:\n%s", dueSection)
	}
	if strings.Contains(dueSection, "Due next year") {
		t.Errorf("a task due next year was listed as due today:\n%s", dueSection)
	}
}

// Nothing due is a real answer and must read as one.
func TestNextTaskSaysWhenNothingIsDue(t *testing.T) {
	srv := vikunjaStub(t, map[int64][]vikunjaTask{1: {{Title: "Someday"}}})
	defer srv.Close()

	out := (&Bot{VikunjaURL: srv.URL, VikunjaToken: "testtoken"}).nextTask(context.Background())
	if !strings.Contains(out, "Nothing due today") {
		t.Errorf("expected an explicit nothing-due line:\n%s", out)
	}
}

// An unreachable Vikunja must not read as an empty task list, which is what
// "you have nothing to do" would look like.
func TestNextTaskDistinguishesFailureFromEmpty(t *testing.T) {
	srv := vikunjaStub(t, nil)
	srv.Close() // refuse connections

	out := (&Bot{VikunjaURL: srv.URL, VikunjaToken: "testtoken"}).nextTask(context.Background())
	if !strings.Contains(out, "could not reach Vikunja") {
		t.Errorf("a dead server did not report as an error:\n%s", out)
	}
	if strings.Contains(out, "Nothing open") {
		t.Errorf("a dead server read as an empty task list:\n%s", out)
	}
}

// A rejected token is the failure this will actually hit, because the token in
// use expires.
func TestNextTaskReportsARejectedToken(t *testing.T) {
	srv := vikunjaStub(t, nil)
	defer srv.Close()

	out := (&Bot{VikunjaURL: srv.URL, VikunjaToken: "wrong"}).nextTask(context.Background())
	if !strings.Contains(out, "could not reach Vikunja") {
		t.Errorf("a 401 did not report as an error:\n%s", out)
	}
}

func TestNextTaskWithoutConfiguration(t *testing.T) {
	out := (&Bot{}).nextTask(context.Background())
	if !strings.Contains(out, "not configured") {
		t.Errorf("an unconfigured bot should say so:\n%s", out)
	}
}
