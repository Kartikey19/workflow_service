// Package schedules owns recurring workflow starts. A schedule is a row whose
// next_run_at says when its next slot is due; workers claim due rows with
// FOR UPDATE SKIP LOCKED and start each slot's run with an idempotency key
// derived from the slot, so a slot starts at most one run however many
// workers race for it, and a crash between starting and recording repeats
// nothing.
package schedules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // schedules need IANA zones even where the OS has none

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/services/workflows"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/db"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/uid"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// Starter starts runs and validates inputs; *runs.Service implements it.
type Starter interface {
	Start(context.Context, runs.StartParams) (runs.StartResult, error)
	CheckInput(ctx context.Context, tenantID pgtype.UUID, workflow string, input map[string]any) error
}

// Grace is how late a slot may start and still count as on time. A later slot
// was missed (workers were down): catch_up decides whether it still runs.
const Grace = 2 * time.Minute

// maxPreview bounds how many upcoming times a preview returns.
const maxPreview = 10

type Service struct {
	pool *pgxpool.Pool
	q    *gen.Queries
	runs Starter
	log  *zap.Logger
	now  func() time.Time
}

func New(pool *pgxpool.Pool, starter Starter, log *zap.Logger) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{pool: pool, q: gen.New(pool), runs: starter, log: log, now: time.Now}
}

// Input creates or changes a schedule. On update, nil and empty fields keep
// their current value.
type Input struct {
	Name     string          `json:"name"`
	Workflow string          `json:"workflow"`
	Cron     string          `json:"cron"`
	Timezone string          `json:"timezone"`
	Input    *map[string]any `json:"input"`
	Overlap  string          `json:"overlap"`
	CatchUp  string          `json:"catch_up"`
	Enabled  *bool           `json:"enabled"`
}

// Info is a schedule as the API shows it.
type Info struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Workflow    string         `json:"workflow"`
	Cron        string         `json:"cron"`
	Timezone    string         `json:"timezone"`
	Input       map[string]any `json:"input"`
	Overlap     string         `json:"overlap"`
	CatchUp     string         `json:"catch_up"`
	Enabled     bool           `json:"enabled"`
	NextRunAt   *time.Time     `json:"next_run_at"`
	LastSlotAt  *time.Time     `json:"last_slot_at"`
	LastRunID   *string        `json:"last_run_id"`
	LastOutcome string         `json:"last_outcome,omitempty"`
	LastError   string         `json:"last_error,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Five standard fields (minute hour day-of-month month day-of-week) plus
// descriptors such as @daily and @every 15m.
var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Parse validates a cron expression and zone and returns the schedule.
func Parse(expression, zone string) (cron.Schedule, *time.Location, error) {
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return nil, nil, exception.BadRequest("cron is required, for example \"0 9 * * 1-5\"")
	}
	if strings.HasPrefix(expression, "TZ=") || strings.HasPrefix(expression, "CRON_TZ=") {
		return nil, nil, exception.BadRequest("set the timezone field instead of TZ= in the expression")
	}
	if zone == "" {
		zone = "UTC"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "Local" {
		return nil, nil, exception.BadRequest(fmt.Sprintf("unknown timezone %q; use an IANA name such as Europe/London", zone))
	}
	schedule, err := parser.Parse(expression)
	if err != nil {
		return nil, nil, exception.BadRequest("invalid cron expression: " + err.Error())
	}
	if every, ok := schedule.(cron.ConstantDelaySchedule); ok && every.Delay < time.Minute {
		return nil, nil, exception.BadRequest("a schedule runs at most once a minute")
	}
	return schedule, loc, nil
}

// Preview returns the next n start times after now, in the schedule's zone.
func (s *Service) Preview(expression, zone string, n int) ([]time.Time, error) {
	schedule, loc, err := Parse(expression, zone)
	if err != nil {
		return nil, err
	}
	if n <= 0 || n > maxPreview {
		n = 5
	}
	out := make([]time.Time, 0, n)
	at := s.now().In(loc)
	for range n {
		at = schedule.Next(at)
		if at.IsZero() {
			break
		}
		out = append(out, at)
	}
	return out, nil
}

func mode(value, def string, allowed ...string) (string, error) {
	if value == "" {
		return def, nil
	}
	for _, a := range allowed {
		if value == a {
			return value, nil
		}
	}
	return "", exception.BadRequest(fmt.Sprintf("must be one of %s", strings.Join(allowed, ", ")))
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func view(row gen.Schedule) Info {
	input := map[string]any{}
	_ = json.Unmarshal(row.Input, &input)
	info := Info{
		ID: uid.String(row.ID), Name: row.Name, Workflow: row.Workflow, Cron: row.Cron, Timezone: row.Timezone,
		Input: input, Overlap: row.Overlap, CatchUp: row.CatchUp, Enabled: row.Enabled,
		NextRunAt: timePtr(row.NextRunAt), LastSlotAt: timePtr(row.LastSlotAt),
		LastOutcome: row.LastOutcome.String, LastError: row.LastError.String, CreatedAt: row.CreatedAt.Time,
	}
	if row.LastRunID.Valid {
		id := uid.String(row.LastRunID)
		info.LastRunID = &id
	}
	return info
}

func (s *Service) nextAfter(expression, zone string, after time.Time) (pgtype.Timestamptz, error) {
	schedule, loc, err := Parse(expression, zone)
	if err != nil {
		return pgtype.Timestamptz{}, err
	}
	next := schedule.Next(after.In(loc))
	if next.IsZero() {
		return pgtype.Timestamptz{}, exception.BadRequest("this cron expression never fires")
	}
	return pgtype.Timestamptz{Time: next, Valid: true}, nil
}

// checkInput maps the run service's validation errors onto field errors the
// schedule form can show.
func (s *Service) checkInput(ctx context.Context, org pgtype.UUID, workflow string, input map[string]any) error {
	err := s.runs.CheckInput(ctx, org, workflow, input)
	switch {
	case errors.Is(err, runs.ErrNotFound):
		return exception.BadRequest("workflow must name a published workflow in this organization")
	case errors.Is(err, runs.ErrArchived):
		return exception.BadRequest("workflow is archived; restore it before scheduling it")
	}
	return err
}

func (s *Service) Create(ctx context.Context, org pgtype.UUID, actor string, in Input) (Info, error) {
	if !namePattern.MatchString(in.Name) {
		return Info{}, exception.BadRequest("a schedule name uses lowercase letters, digits, - and _")
	}
	overlap, err := mode(in.Overlap, "skip", "skip", "allow")
	if err != nil {
		return Info{}, err
	}
	catchUp, err := mode(in.CatchUp, "latest", "latest", "skip")
	if err != nil {
		return Info{}, err
	}
	if in.Timezone == "" {
		in.Timezone = "UTC"
	}
	input := map[string]any{}
	if in.Input != nil && *in.Input != nil {
		input = *in.Input
	}
	next, err := s.nextAfter(in.Cron, in.Timezone, s.now())
	if err != nil {
		return Info{}, err
	}
	if err := s.checkInput(ctx, org, in.Workflow, input); err != nil {
		return Info{}, err
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return Info{}, exception.BadRequest("input must be a JSON object")
	}
	enabled := in.Enabled == nil || *in.Enabled
	if !enabled {
		next = pgtype.Timestamptz{}
	}
	row, err := s.q.CreateSchedule(ctx, gen.CreateScheduleParams{
		ID: uid.New(), OrgID: org, Name: in.Name, Workflow: in.Workflow, Cron: strings.TrimSpace(in.Cron),
		Timezone: in.Timezone, Input: encoded, Overlap: overlap, CatchUp: catchUp, Enabled: enabled,
		NextRunAt: next, CreatedBy: pgtype.Text{String: actor, Valid: actor != ""},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Info{}, exception.Conflict("name_taken", "a schedule with this name already exists")
	}
	if err != nil {
		return Info{}, fmt.Errorf("create schedule: %w", err)
	}
	return view(row), nil
}

func (s *Service) List(ctx context.Context, org pgtype.UUID, workflow string) ([]Info, error) {
	rows, err := s.q.ListSchedules(ctx, gen.ListSchedulesParams{OrgID: org, Workflow: pgtype.Text{String: workflow, Valid: workflow != ""}})
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	out := make([]Info, 0, len(rows))
	for _, row := range rows {
		out = append(out, view(row))
	}
	return out, nil
}

func (s *Service) get(ctx context.Context, org, id pgtype.UUID) (gen.Schedule, error) {
	row, err := s.q.GetSchedule(ctx, gen.GetScheduleParams{OrgID: org, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Schedule{}, exception.NotFound("schedule")
	}
	if err != nil {
		return gen.Schedule{}, fmt.Errorf("get schedule: %w", err)
	}
	return row, nil
}

// Update changes a schedule. A changed expression, zone or re-enabling moves
// the next slot to the first one from now; otherwise the pending slot stays.
func (s *Service) Update(ctx context.Context, org, id pgtype.UUID, in Input) (Info, error) {
	row, err := s.get(ctx, org, id)
	if err != nil {
		return Info{}, err
	}
	reschedule := false
	if in.Cron != "" && strings.TrimSpace(in.Cron) != row.Cron {
		row.Cron, reschedule = strings.TrimSpace(in.Cron), true
	}
	if in.Timezone != "" && in.Timezone != row.Timezone {
		row.Timezone, reschedule = in.Timezone, true
	}
	if row.Overlap, err = mode(in.Overlap, row.Overlap, "skip", "allow"); err != nil {
		return Info{}, err
	}
	if row.CatchUp, err = mode(in.CatchUp, row.CatchUp, "latest", "skip"); err != nil {
		return Info{}, err
	}
	if in.Enabled != nil && *in.Enabled != row.Enabled {
		row.Enabled, reschedule = *in.Enabled, true
	}
	if in.Input != nil {
		input := *in.Input
		if input == nil {
			input = map[string]any{}
		}
		if err := s.checkInput(ctx, org, row.Workflow, input); err != nil {
			return Info{}, err
		}
		if row.Input, err = json.Marshal(input); err != nil {
			return Info{}, exception.BadRequest("input must be a JSON object")
		}
	}
	next, err := s.nextAfter(row.Cron, row.Timezone, s.now())
	if err != nil {
		return Info{}, err
	}
	switch {
	case !row.Enabled:
		row.NextRunAt = pgtype.Timestamptz{}
	case reschedule || !row.NextRunAt.Valid:
		row.NextRunAt = next
	}
	updated, err := s.q.UpdateSchedule(ctx, gen.UpdateScheduleParams{
		OrgID: org, ID: id, Cron: row.Cron, Timezone: row.Timezone, Input: row.Input,
		Overlap: row.Overlap, CatchUp: row.CatchUp, Enabled: row.Enabled, NextRunAt: row.NextRunAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Info{}, exception.NotFound("schedule")
	}
	if err != nil {
		return Info{}, fmt.Errorf("update schedule: %w", err)
	}
	return view(updated), nil
}

func (s *Service) Delete(ctx context.Context, org, id pgtype.UUID) error {
	n, err := s.q.DeleteSchedule(ctx, gen.DeleteScheduleParams{OrgID: org, ID: id})
	if err != nil {
		return fmt.Errorf("delete schedule: %w", err)
	}
	if n == 0 {
		return exception.NotFound("schedule")
	}
	return nil
}

// RunNow starts the schedule's workflow with its input immediately. It does
// not move the next slot.
func (s *Service) RunNow(ctx context.Context, org, id pgtype.UUID, actor string) (runs.StartResult, error) {
	row, err := s.get(ctx, org, id)
	if err != nil {
		return runs.StartResult{}, err
	}
	input := map[string]any{}
	_ = json.Unmarshal(row.Input, &input)
	result, err := s.runs.Start(ctx, runs.StartParams{
		TenantID: org, Workflow: row.Workflow, Input: input, Trigger: "schedule", CreatedBy: actor,
	})
	if err != nil {
		return runs.StartResult{}, err
	}
	if err := s.q.RecordScheduleRun(ctx, gen.RecordScheduleRunParams{OrgID: org, ID: id, LastRunID: result.Run.ID}); err != nil {
		s.log.Warn("record manual schedule run", zap.Error(err), zap.String("schedule_id", uid.String(id)))
	}
	return result, nil
}

// slotFor picks the slot a due schedule fires for. "latest" collapses every
// missed slot into the most recent one; "skip" fires only an on-time slot.
// It returns the slot, whether to start a run, and the next slot after now.
func slotFor(schedule cron.Schedule, loc *time.Location, due time.Time, catchUp string, now time.Time) (slot time.Time, fire bool, next time.Time) {
	slot = due
	// Walk forward to the latest slot not after now. The bound keeps a
	// once-a-minute schedule that was off for weeks from looping for long.
	for range 100_000 {
		following := schedule.Next(slot.In(loc))
		if following.IsZero() || following.After(now) {
			break
		}
		slot = following
	}
	fire = catchUp == "latest" || now.Sub(slot) <= Grace
	next = schedule.Next(now.In(loc))
	return slot, fire, next
}

// Dispatch starts every due slot. Workers call it from the sweeper; it returns
// how many schedules it handled.
func (s *Service) Dispatch(ctx context.Context) (int, error) {
	handled := 0
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		due, err := q.ClaimDueSchedules(ctx, 20)
		if err != nil {
			return fmt.Errorf("claim due schedules: %w", err)
		}
		for _, row := range due {
			if err := s.fire(ctx, q, row); err != nil {
				return err
			}
			handled++
		}
		return nil
	})
	return handled, err
}

func (s *Service) fire(ctx context.Context, q *gen.Queries, row gen.Schedule) error {
	now := s.now()
	record := gen.RecordScheduleSlotParams{ID: row.ID, LastSlotAt: row.NextRunAt}
	schedule, loc, err := Parse(row.Cron, row.Timezone)
	if err != nil {
		// Saved expressions were valid; a zone database change could break one.
		record.NextRunAt = pgtype.Timestamptz{Time: now.Add(time.Hour), Valid: true}
		record.LastOutcome = "error"
		record.LastError = pgtype.Text{String: "schedule can no longer be parsed: " + err.Error(), Valid: true}
		return q.RecordScheduleSlot(ctx, record)
	}
	slot, fire, next := slotFor(schedule, loc, row.NextRunAt.Time, row.CatchUp, now)
	record.LastSlotAt = pgtype.Timestamptz{Time: slot, Valid: true}
	record.NextRunAt = pgtype.Timestamptz{Time: next, Valid: !next.IsZero()}
	if !record.NextRunAt.Valid {
		record.NextRunAt = pgtype.Timestamptz{Time: now.AddDate(100, 0, 0), Valid: true}
	}
	alert := func(reason string) error {
		return runs.EnqueueAlert(ctx, q, row.OrgID, row.Workflow, runs.AlertScheduleFailed,
			runs.AlertScheduleFailed+":"+uid.String(row.ID)+":"+strconv.FormatInt(slot.Unix(), 10),
			map[string]any{"workflow": row.Workflow, "schedule": row.Name, "slot": slot.UTC().Format(time.RFC3339), "message": reason})
	}
	switch {
	case !fire:
		record.LastOutcome = "missed"
		record.LastError = pgtype.Text{String: fmt.Sprintf("the %s slot was missed by %s and catch-up is off", slot.In(loc).Format(time.RFC3339), now.Sub(slot).Round(time.Second)), Valid: true}
		if err := alert(record.LastError.String); err != nil {
			return err
		}
	case row.Overlap == "skip" && s.stillRunning(ctx, q, row):
		record.LastOutcome = "skipped"
		record.LastError = pgtype.Text{String: "the previous run was still in progress", Valid: true}
	default:
		input := map[string]any{}
		_ = json.Unmarshal(row.Input, &input)
		result, err := s.runs.Start(ctx, runs.StartParams{
			TenantID: row.OrgID, Workflow: row.Workflow, Input: input, Trigger: "schedule",
			CreatedBy:      "schedule:" + row.Name,
			IdempotencyKey: "schedule:" + uid.String(row.ID) + ":" + strconv.FormatInt(slot.Unix(), 10),
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			record.LastOutcome = "error"
			record.LastError = pgtype.Text{String: describe(err), Valid: true}
			s.log.Warn("scheduled run did not start", zap.String("schedule_id", uid.String(row.ID)), zap.Error(err))
			if err := alert(record.LastError.String); err != nil {
				return err
			}
			break
		}
		record.LastOutcome = "started"
		record.LastRunID = result.Run.ID
	}
	if err := q.RecordScheduleSlot(ctx, record); err != nil {
		return fmt.Errorf("record schedule slot: %w", err)
	}
	return nil
}

func (s *Service) stillRunning(ctx context.Context, q *gen.Queries, row gen.Schedule) bool {
	if !row.LastRunID.Valid {
		return false
	}
	run, err := q.GetRun(ctx, gen.GetRunParams{ID: row.LastRunID, TenantID: row.OrgID})
	return err == nil && !runs.IsTerminalRun(run.Status)
}

// describe turns a start failure into text safe to show the organization.
func describe(err error) string {
	var input *runs.InputError
	var limit *workflows.LimitError
	var problem *exception.Exception
	switch {
	case errors.As(err, &input):
		parts := make([]string, 0, len(input.Fields))
		for _, f := range input.Fields {
			parts = append(parts, f.Field+": "+f.Message)
		}
		return "input is no longer valid: " + strings.Join(parts, "; ")
	case errors.Is(err, runs.ErrNotFound):
		return "the workflow no longer exists or has no published version"
	case errors.Is(err, runs.ErrArchived):
		return "the workflow is archived"
	case errors.As(err, &limit):
		return limit.Error()
	case errors.As(err, &problem):
		return problem.Detail
	}
	return "the run could not be started"
}
