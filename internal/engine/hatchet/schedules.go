package hatchet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hatchet-dev/hatchet/pkg/client/rest"
	sdk "github.com/hatchet-dev/hatchet/sdks/go"
	"github.com/hatchet-dev/hatchet/sdks/go/features"
	"github.com/robfig/cron/v3"

	"git.phin.tech/sam/starci/internal/forge"
	"git.phin.tech/sam/starci/internal/plan"
	"git.phin.tech/sam/starci/internal/trigger"
)

// A schedule a script declares becomes a Hatchet cron. When the cron fires,
// Hatchet starts ci-scheduled, which looks at the repository as it is at that
// moment, asks the script what the schedule wants done, and starts the run.
// The cron itself knows only which schedule of which repository it is.

// ScheduledTask is the task a cron starts.
const ScheduledTask = "ci-scheduled"

// ScheduledInput is all a cron carries.
type ScheduledInput struct {
	Forge    string `json:"forge"`
	Repo     string `json:"repo"`
	Schedule string `json:"schedule"`
	// Overlap is what the schedule asked for when the cron was made. It is
	// carried here rather than read from the script again, so that a firing
	// behaves as the cron was set up to even while the script is changing.
	Overlap string `json:"overlap"`
	// CatchUp is likewise as declared when the cron was made.
	CatchUp bool `json:"catch_up"`
}

func (in ScheduledInput) key() string { return in.Forge + ":" + in.Repo + ":" + in.Schedule }

// crons keeps Hatchet's crons in step with what scripts declare.
type crons struct {
	client *sdk.Client
}

// all lists the crons that start ci-scheduled.
func (c *crons) all(ctx context.Context) ([]rest.CronWorkflows, error) {
	name, limit := ScheduledTask, int64(1000)
	list, err := c.client.Crons().List(ctx, rest.CronWorkflowListParams{WorkflowName: &name, Limit: &limit})
	if err != nil {
		return nil, err
	}
	if list.Rows == nil {
		return nil, nil
	}
	return *list.Rows, nil
}

// inputOf reads back what a cron carries.
func inputOf(row rest.CronWorkflows) (ScheduledInput, bool) {
	var in ScheduledInput
	if row.Input == nil {
		return in, false
	}
	data, err := json.Marshal(*row.Input)
	if err != nil || json.Unmarshal(data, &in) != nil || in.Repo == "" || in.Schedule == "" {
		return in, false
	}
	return in, true
}

// Sync makes the crons for a repository exactly the schedules given.
func (c *crons) Sync(ctx context.Context, forgeName, repo string, schedules []plan.Schedule) error {
	existing, err := c.all(ctx)
	if err != nil {
		return fmt.Errorf("listing schedules: %w", err)
	}
	want := map[string]plan.Schedule{}
	for _, s := range schedules {
		want[s.Name] = s
	}

	kept := map[string]bool{}
	for _, row := range existing {
		in, ok := inputOf(row)
		if !ok || in.Forge != forgeName || in.Repo != repo {
			continue
		}
		s, wanted := want[in.Schedule]
		// Unchanged ones are left alone; a cron that is deleted and made
		// again in the minute it was due would not fire.
		if wanted && !kept[in.Schedule] && row.Cron == s.Cron && in.Overlap == s.Overlap && in.CatchUp == s.CatchUp {
			kept[in.Schedule] = true
			continue
		}
		if err := c.client.Crons().Delete(ctx, row.Metadata.Id); err != nil {
			return fmt.Errorf("removing schedule %s: %w", in.Schedule, err)
		}
	}
	for _, s := range schedules {
		if kept[s.Name] {
			continue
		}
		in := ScheduledInput{Forge: forgeName, Repo: repo, Schedule: s.Name, Overlap: s.Overlap, CatchUp: s.CatchUp}
		var input map[string]any
		data, _ := json.Marshal(in)
		_ = json.Unmarshal(data, &input)
		_, err := c.client.Crons().Create(ctx, ScheduledTask, features.CreateCronTrigger{
			Name:       cronName(in),
			Expression: s.Cron,
			Input:      input,
		})
		if err != nil {
			return fmt.Errorf("adding schedule %s: %w", s.Name, err)
		}
	}
	return nil
}

// cronName is what the cron is called in Hatchet. Readable, since it is what
// the dashboard shows, and made of characters nothing objects to.
func cronName(in ScheduledInput) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
				return r
			}
			return '-'
		}, s)
	}
	return "starci-" + clean(in.Forge) + "-" + clean(in.Repo) + "-" + clean(in.Schedule)
}

// remove deletes the cron for one schedule, when a firing finds there is
// nothing left for it to do.
func (c *crons) remove(ctx context.Context, in ScheduledInput) {
	rows, err := c.all(ctx)
	if err != nil {
		return
	}
	for _, row := range rows {
		if got, ok := inputOf(row); ok && got.key() == in.key() {
			_ = c.client.Crons().Delete(ctx, row.Metadata.Id)
		}
	}
}

func scheduledTask(client *sdk.Client, receiver *trigger.Receiver, timers *crons, post Reporter, stateDir string) *sdk.StandaloneTask {
	return client.NewStandaloneTask(ScheduledTask,
		func(ctx sdk.Context, in ScheduledInput) (DeliveryResult, error) {
			run, found, err := receiver.Scheduled(ctx, in.Forge, in.Repo, in.Schedule)
			if err != nil {
				// The forge could not be asked. Tried again; the schedule stays.
				return DeliveryResult{}, err
			}
			if !found {
				timers.remove(ctx, in)
				return DeliveryResult{Said: "nothing declares this schedule any more; removed"}, nil
			}
			// Noted before the run starts: this firing has happened, however
			// the run goes.
			noteFired(stateDir, in, time.Now())

			say := func(s forge.Status) {
				if post == nil {
					ctx.Log("this worker has no forge credentials; the outcome was not posted to " + in.Repo)
					return
				}
				if err := post(ctx, *run.Report, s); err != nil {
					ctx.Log("could not post the outcome: " + err.Error())
				}
			}
			if run.Broken != "" {
				say(forge.Status{State: "failure", Description: run.Broken})
				return DeliveryResult{Said: "the script is broken: " + run.Broken}, nil
			}
			if run.Mode == plan.Basic && len(run.Plan.Steps) == 0 {
				say(forge.Status{State: "success", Description: "nothing to run for this schedule"})
				return DeliveryResult{Said: "the plan is empty for this schedule"}, nil
			}

			// Which of two runs of a schedule gives way is the schedule's
			// choice. No status is posted here: a firing that is skipped
			// never starts, and would leave "pending" behind for good.
			switch in.Overlap {
			case plan.OverlapReplace:
				// run.Supersedes already names the schedule: the newer wins.
			case plan.OverlapAllow:
				run.Supersedes = OrUnique("")
			default:
				run.Exclusive, run.Supersedes = run.Supersedes, OrUnique("")
			}
			if err := receiver.Submit.Start(ctx, run); err != nil {
				return DeliveryResult{}, fmt.Errorf("starting the run: %w", err)
			}
			return DeliveryResult{Said: "started"}, nil
		},
		sdk.WithRetries(3),
	)
}

// When each schedule last fired is kept on the worker, one small file each.
// It is what catching up is measured against.

func firedPath(stateDir string, in ScheduledInput) string {
	sum := sha256.Sum256([]byte(in.key()))
	return filepath.Join(stateDir, "schedules", hex.EncodeToString(sum[:])[:32]+".last")
}

func noteFired(stateDir string, in ScheduledInput, at time.Time) {
	if stateDir == "" {
		return
	}
	path := firedPath(stateDir, in)
	if os.MkdirAll(filepath.Dir(path), 0o755) == nil {
		_ = os.WriteFile(path, []byte(at.UTC().Format(time.RFC3339)), 0o644)
	}
}

func lastFired(stateDir string, in ScheduledInput) (time.Time, bool) {
	data, err := os.ReadFile(firedPath(stateDir, in))
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	return at, err == nil
}

// grace is how long after a firing was due before it counts as missed. The
// engine's own firing needs a moment to be noted.
const grace = 3 * time.Minute

// missed reports whether a schedule that last fired at last had a firing due
// since, long enough ago that it is not simply about to happen.
func missed(expression string, last, now time.Time) (bool, error) {
	schedule, err := cron.ParseStandard(expression)
	if err != nil {
		return false, err
	}
	due := schedule.Next(last.UTC())
	return !due.After(now.UTC().Add(-grace)), nil
}

// CatchUp starts, once each, the schedules that asked to be caught up and
// have a firing they missed: one that was due while nothing was there to
// fire it. It is for calling when a worker starts and every so often after.
//
// A schedule seen for the first time is not caught up; there is nothing to
// say it missed anything. The record of firings is this worker's own, so
// this is for one worker to do.
func (t Tasks) CatchUp(ctx context.Context, client *sdk.Client, stateDir string, now time.Time) (started []string, err error) {
	if t.timers == nil || stateDir == "" {
		return nil, nil
	}
	rows, err := t.timers.all(ctx)
	if err != nil {
		return nil, err
	}
	var scheduled *sdk.StandaloneTask
	for _, r := range t.receiving {
		if r.GetName() == ScheduledTask {
			scheduled = r
		}
	}
	if scheduled == nil {
		return nil, errors.New("this worker does not fire schedules")
	}
	for _, row := range rows {
		in, ok := inputOf(row)
		if !ok || !in.CatchUp || !row.Enabled {
			continue
		}
		last, seen := lastFired(stateDir, in)
		if !seen {
			noteFired(stateDir, in, now)
			continue
		}
		late, err := missed(row.Cron, last, now)
		if err != nil || !late {
			continue
		}
		// Noted first, so that a schedule is caught up once however many
		// firings it missed and however often this is called.
		noteFired(stateDir, in, now)
		if _, err := scheduled.RunNoWait(ctx, in); err != nil {
			return started, fmt.Errorf("catching up %s: %w", in.Schedule, err)
		}
		started = append(started, in.key())
	}
	return started, nil
}
