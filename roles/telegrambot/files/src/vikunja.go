package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Vikunja writes this when a task has no due date, so it arrives as a valid
// timestamp in the year 1 rather than as null.
const vikunjaNoDueDate = "0001-01-01T00:00:00Z"

type vikunjaView struct {
	ID   int64  `json:"id"`
	Kind string `json:"view_kind"`
}

type vikunjaProject struct {
	ID       int64         `json:"id"`
	Title    string        `json:"title"`
	Archived bool          `json:"is_archived"`
	Views    []vikunjaView `json:"views"`
}

type vikunjaTask struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Done    bool   `json:"done"`
	DueDate string `json:"due_date"`
}

// listViewID is the view the web interface opens a project on, and the order
// its tasks come back in is the order shown there. Asking for that view is
// what makes "the top task" mean the one at the top of the list, rather than
// whichever the database returned first.
func (p vikunjaProject) listViewID() (int64, bool) {
	for _, v := range p.Views {
		if v.Kind == "list" {
			return v.ID, true
		}
	}
	return 0, false
}

// dueOn reports whether the task is due on the given day, in the day's own
// location. Tasks with no due date carry a year-1 timestamp rather than an
// empty string, which would otherwise compare as a real date.
func (t vikunjaTask) dueOn(day time.Time) bool {
	if t.DueDate == "" || t.DueDate == vikunjaNoDueDate {
		return false
	}
	due, err := time.Parse(time.RFC3339, t.DueDate)
	if err != nil {
		return false
	}
	due = due.In(day.Location())
	dy, dm, dd := due.Date()
	y, m, d := day.Date()
	return dy == y && dm == m && dd == d
}

func (b *Bot) vikunjaGet(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(b.VikunjaURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.VikunjaToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The body carries Vikunja's own error message, which says more than
		// the status alone -- an expired token reads as 401 with "missing or
		// malformed token".
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type projectTasks struct {
	Project string
	Tasks   []vikunjaTask
}

// vikunjaOpenTasks returns each unarchived project with the open tasks its
// list view holds, in that view's order.
func (b *Bot) vikunjaOpenTasks(ctx context.Context) ([]projectTasks, error) {
	var projects []vikunjaProject
	if err := b.vikunjaGet(ctx, "/api/v1/projects", &projects); err != nil {
		return nil, err
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })

	var out []projectTasks
	for _, p := range projects {
		if p.Archived {
			continue
		}
		view, ok := p.listViewID()
		if !ok {
			continue
		}
		var tasks []vikunjaTask
		if err := b.vikunjaGet(ctx, fmt.Sprintf("/api/v1/projects/%d/views/%d/tasks", p.ID, view), &tasks); err != nil {
			return nil, err
		}
		// The view endpoint already excludes done tasks, but a task completed
		// between the two requests would still arrive marked done.
		open := tasks[:0]
		for _, t := range tasks {
			if !t.Done {
				open = append(open, t)
			}
		}
		out = append(out, projectTasks{Project: p.Title, Tasks: open})
	}
	return out, nil
}

// nextTask answers with the first open task of every project, and everything
// due today. A task can appear in both: they answer different questions.
func (b *Bot) nextTask(ctx context.Context) string {
	if b.VikunjaURL == "" || b.VikunjaToken == "" {
		return "Vikunja is not configured for this bot."
	}

	projects, err := b.vikunjaOpenTasks(ctx)
	if err != nil {
		// Distinct from "nothing to do", which is what an empty list would
		// otherwise be mistaken for.
		return "❌ could not reach Vikunja: " + htmlEscape(err.Error())
	}

	var sb strings.Builder
	sb.WriteString("<b>Next task per project</b>\n")
	any := false
	for _, p := range projects {
		if len(p.Tasks) == 0 {
			continue
		}
		any = true
		sb.WriteString(fmt.Sprintf("• %s — %s\n", htmlEscape(p.Project), htmlEscape(p.Tasks[0].Title)))
	}
	if !any {
		sb.WriteString("Nothing open.\n")
	}

	today := time.Now()
	var due []string
	for _, p := range projects {
		for _, t := range p.Tasks {
			if t.dueOn(today) {
				due = append(due, fmt.Sprintf("• %s — %s", htmlEscape(p.Project), htmlEscape(t.Title)))
			}
		}
	}

	sb.WriteString("\n<b>Due today</b>\n")
	if len(due) == 0 {
		sb.WriteString("Nothing due today.\n")
	} else {
		sb.WriteString(strings.Join(due, "\n"))
		sb.WriteString("\n")
	}
	return sb.String()
}
