// Package tasks is the application service behind the human-task worklist
// (02 §4.7). The authoritative state of a task is the wait it parks on:
// completing a task delivers that wait's signal, so everything the signal path
// guarantees (payload validation, exactly-once delivery, the run advancing in
// one transaction) holds for tasks too.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/exception"
)

// Task statuses, derived from the wait the task parks on.
const (
	StatusOpen      = "open"
	StatusClaimed   = "claimed"
	StatusCompleted = "completed"
	StatusExpired   = "expired"
	StatusCancelled = "cancelled"
)

// Actor is who is acting on a task. Admin lets an organization admin act on
// tasks assigned to or claimed by someone else.
type Actor struct {
	Name  string
	Admin bool
}

// Service is safe for concurrent use.
type Service struct {
	q    *gen.Queries
	runs *runs.Service
}

func NewService(r *runs.Service) *Service { return &Service{q: r.Q, runs: r} }

// Filter selects tasks for the worklist; empty fields do not filter.
type Filter struct {
	TenantID      pgtype.UUID
	Queue         string
	Status        string
	Assignee      string
	ClaimedBy     string
	BeforeCreated pgtype.Timestamptz
	BeforeID      pgtype.UUID
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// List returns up to limit tasks, newest first, and whether more follow.
func (s *Service) List(ctx context.Context, f Filter, limit int) ([]gen.TaskDetail, bool, error) {
	rows, err := s.q.ListTaskDetails(ctx, gen.ListTaskDetailsParams{
		TenantID: f.TenantID, Queue: text(f.Queue), Status: text(f.Status), Assignee: text(f.Assignee),
		ClaimedBy: text(f.ClaimedBy), BeforeCreated: f.BeforeCreated, BeforeID: f.BeforeID, Lim: int32(limit + 1),
	})
	if err != nil {
		return nil, false, fmt.Errorf("list tasks: %w", err)
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	return rows, more, nil
}

// Get returns one task. Another organization's task is a 404.
func (s *Service) Get(ctx context.Context, tenantID, id pgtype.UUID) (gen.TaskDetail, error) {
	task, err := s.q.GetTaskDetail(ctx, gen.GetTaskDetailParams{ID: id, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.TaskDetail{}, exception.NotFound("task")
	}
	if err != nil {
		return gen.TaskDetail{}, fmt.Errorf("get task: %w", err)
	}
	return task, nil
}

func (a Actor) mayWork(task gen.TaskDetail) error {
	if task.Assignee.Valid && task.Assignee.String != a.Name && !a.Admin {
		return exception.Forbidden("this task is assigned to someone else")
	}
	return nil
}

func closed(task gen.TaskDetail) error {
	return exception.Conflict("task_closed", fmt.Sprintf("the task is %s", task.Status))
}

// Claim reserves an open task for actor. Claiming a task you already hold is a
// no-op; a task claimed by someone else is a conflict.
func (s *Service) Claim(ctx context.Context, tenantID, id pgtype.UUID, actor Actor) (gen.TaskDetail, error) {
	task, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return gen.TaskDetail{}, err
	}
	if err := actor.mayWork(task); err != nil {
		return gen.TaskDetail{}, err
	}
	switch {
	case task.Status == StatusClaimed && task.ClaimedBy.String == actor.Name:
		return task, nil
	case task.Status == StatusClaimed:
		return gen.TaskDetail{}, exception.Conflict("task_claimed", "the task is claimed by "+task.ClaimedBy.String)
	case task.Status != StatusOpen:
		return gen.TaskDetail{}, closed(task)
	}
	rows, err := s.q.ClaimTask(ctx, gen.ClaimTaskParams{ID: id, TenantID: tenantID, Actor: text(actor.Name)})
	if err != nil {
		return gen.TaskDetail{}, fmt.Errorf("claim task: %w", err)
	}
	if rows == 0 { // lost a race with another claimer or with completion
		return gen.TaskDetail{}, exception.Conflict("task_claimed", "the task was claimed or closed by someone else")
	}
	return s.Get(ctx, tenantID, id)
}

// Release gives a claimed task back to the queue. Only the claimer, or an
// admin, may release it.
func (s *Service) Release(ctx context.Context, tenantID, id pgtype.UUID, actor Actor) (gen.TaskDetail, error) {
	task, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return gen.TaskDetail{}, err
	}
	switch {
	case task.Status == StatusOpen:
		return task, nil
	case task.Status != StatusClaimed:
		return gen.TaskDetail{}, closed(task)
	}
	rows, err := s.q.ReleaseTask(ctx, gen.ReleaseTaskParams{ID: id, TenantID: tenantID, Actor: text(actor.Name), Force: actor.Admin})
	if err != nil {
		return gen.TaskDetail{}, fmt.Errorf("release task: %w", err)
	}
	if rows == 0 {
		return gen.TaskDetail{}, exception.Forbidden("only " + task.ClaimedBy.String + " or an admin can release this task")
	}
	return s.Get(ctx, tenantID, id)
}

// Complete finishes a task by delivering payload as the signal of its wait; the
// payload becomes the node's output and the run advances. A task claimed by
// someone else can only be completed by an admin.
func (s *Service) Complete(ctx context.Context, tenantID, id pgtype.UUID, actor Actor, payload json.RawMessage) (runs.SignalResult, error) {
	task, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return runs.SignalResult{}, err
	}
	if err := actor.mayWork(task); err != nil {
		return runs.SignalResult{}, err
	}
	switch {
	case task.Status == StatusClaimed && task.ClaimedBy.String != actor.Name && !actor.Admin:
		return runs.SignalResult{}, exception.Conflict("task_claimed", "the task is claimed by "+task.ClaimedBy.String)
	case task.Status != StatusOpen && task.Status != StatusClaimed:
		return runs.SignalResult{}, closed(task)
	}
	result, err := s.runs.Signal(ctx, tenantID, task.WaitKey, payload)
	if errors.Is(err, runs.ErrNoWaiter) {
		return runs.SignalResult{}, closed(task) // expired or cancelled since we looked
	}
	if err != nil {
		return runs.SignalResult{}, err
	}
	if err := s.q.SetTaskCompletedBy(ctx, gen.SetTaskCompletedByParams{ID: id, TenantID: tenantID, Actor: text(actor.Name)}); err != nil {
		return result, fmt.Errorf("record task completion: %w", err)
	}
	return result, nil
}
