// Package agents manages an organization's reusable agent definitions: a named
// configuration of provider connection, model, system prompt, input and output
// schemas, tools and limits (05 §3.1, M13). Workflows run one with
// `type: agent, config: {agent: <name>, input: {...}}`; this service is also the
// executor's source of definitions.
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/internal/core/executor/agent"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/uid"
)

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	hostPattern = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
)

// testTimeout bounds one test run.
const testTimeout = 2 * time.Minute

// Service is safe for concurrent use. Attach must be called before Test or
// Create/Update (it supplies the validation and the executor to run).
type Service struct {
	q     *gen.Queries
	tools *agent.Tools

	validator interface{ ValidateDefinition(agent.Definition) error }
	runner    executor.Executor
}

var _ agent.Definitions = (*Service)(nil)

func NewService(pool *pgxpool.Pool, tools *agent.Tools) *Service {
	return &Service{q: gen.New(pool), tools: tools}
}

// Attach connects the agent executor, which validates definitions and runs
// tests. It exists because the executor itself needs this service to load
// definitions.
func (s *Service) Attach(e *agent.Executor) { s.validator, s.runner = e, e }

// Agent is a stored definition as the API shows it.
type Agent struct {
	ID                 pgtype.UUID
	Name               string
	Description        string
	Connection         string
	Model              string
	System             string
	InputSchema        json.RawMessage
	OutputSchema       json.RawMessage
	Tools              []string
	DatabaseConnection string
	AllowedHosts       []string
	MaxIterations      int
	MaxTokens          *int
	Temperature        *float64
	Revision           int
	CreatedAt          time.Time
	UpdatedAt          time.Time
	UpdatedBy          string
}

// Input is what a caller sends to create or replace an agent.
type Input struct {
	Name               string
	Description        string
	Connection         string // provider connection name; empty uses the platform provider
	Model              string
	System             string
	InputSchema        json.RawMessage
	OutputSchema       json.RawMessage
	Tools              []string
	DatabaseConnection string
	AllowedHosts       []string
	MaxIterations      int
	MaxTokens          *int
	Temperature        *float64
}

func nonNull(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

func intPtr(v pgtype.Int4) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int32)
	return &n
}

func floatPtr(v pgtype.Float8) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

func fromRow(a gen.Agent, connection string) Agent {
	return Agent{
		ID: a.ID, Name: a.Name, Description: a.Description, Connection: connection, Model: a.Model, System: a.SystemPrompt,
		InputSchema: a.InputSchema, OutputSchema: a.OutputSchema, Tools: a.Tools, AllowedHosts: a.AllowedHosts, DatabaseConnection: a.DatabaseConnection,
		MaxIterations: int(a.MaxIterations), MaxTokens: intPtr(a.MaxTokens), Temperature: floatPtr(a.Temperature),
		Revision: int(a.Revision), CreatedAt: a.CreatedAt.Time, UpdatedAt: a.UpdatedAt.Time, UpdatedBy: a.UpdatedBy.String,
	}
}

// validate checks a definition and resolves its connection name to an ID.
func (s *Service) validate(ctx context.Context, orgID pgtype.UUID, in *Input) (pgtype.UUID, error) {
	in.Name = strings.TrimSpace(in.Name)
	if !namePattern.MatchString(in.Name) {
		return pgtype.UUID{}, exception.BadRequest("name must start with a lowercase letter and use lowercase letters, digits, - or _ (at most 63 characters)")
	}
	if len(in.Description) > 2000 || len(in.System) > 100_000 {
		return pgtype.UUID{}, exception.BadRequest("description or system prompt is too long")
	}
	if in.Tools == nil {
		in.Tools = []string{}
	}
	if in.AllowedHosts == nil {
		in.AllowedHosts = []string{}
	}
	for i, host := range in.AllowedHosts {
		in.AllowedHosts[i] = strings.ToLower(strings.TrimSpace(host))
		if !hostPattern.MatchString(in.AllowedHosts[i]) {
			return pgtype.UUID{}, exception.BadRequest(fmt.Sprintf("allowed_hosts entry %q must be a host name such as api.example.com or *.example.com", host))
		}
	}
	definition := agent.Definition{
		Name: in.Name, Connection: in.Connection, Model: strings.TrimSpace(in.Model), System: in.System,
		InputSchema: in.InputSchema, OutputSchema: in.OutputSchema, Tools: in.Tools, AllowedHosts: in.AllowedHosts, DatabaseConnection: in.DatabaseConnection,
		MaxIterations: in.MaxIterations, Temperature: in.Temperature,
	}
	if in.MaxTokens != nil {
		definition.MaxTokens = *in.MaxTokens
	}
	if s.validator != nil {
		if err := s.validator.ValidateDefinition(definition); err != nil {
			return pgtype.UUID{}, exception.BadRequest(err.Error())
		}
	}
	in.Model = definition.Model
	if in.DatabaseConnection != "" {
		row, err := s.q.GetConnection(ctx, gen.GetConnectionParams{OrgID: orgID, Name: in.DatabaseConnection})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && row.Kind != "postgres" && row.Kind != "mysql" {
			return pgtype.UUID{}, exception.BadRequest("database_connection must name a postgres or mysql connection in this organization")
		}
		if err != nil {
			return pgtype.UUID{}, fmt.Errorf("load database connection: %w", err)
		}
	}
	for _, ref := range in.Tools {
		if !agent.IsMCPReference(ref) {
			continue
		}
		row, err := s.q.GetConnection(ctx, gen.GetConnectionParams{OrgID: orgID, Name: strings.TrimPrefix(ref, "mcp:")})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && row.Kind != "mcp" {
			return pgtype.UUID{}, exception.BadRequest("MCP tool reference must name an mcp connection in this organization")
		}
		if err != nil {
			return pgtype.UUID{}, fmt.Errorf("load MCP connection: %w", err)
		}
	}
	if in.Connection == "" {
		return pgtype.UUID{}, nil
	}
	connection, err := s.q.GetProviderConnectionByName(ctx, gen.GetProviderConnectionByNameParams{OrgID: orgID, Name: in.Connection})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, exception.BadRequest(fmt.Sprintf("provider connection %q does not exist", in.Connection))
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("load provider connection: %w", err)
	}
	return connection.ID, nil
}

// Create stores a new agent.
func (s *Service) Create(ctx context.Context, orgID pgtype.UUID, actor string, in Input) (Agent, error) {
	connectionID, err := s.validate(ctx, orgID, &in)
	if err != nil {
		return Agent{}, err
	}
	row, err := s.q.CreateAgent(ctx, gen.CreateAgentParams{
		ID: uid.New(), OrgID: orgID, Name: in.Name, Description: in.Description, ConnectionID: connectionID,
		Model: in.Model, SystemPrompt: in.System, InputSchema: nonNull(in.InputSchema), OutputSchema: nonNull(in.OutputSchema),
		Tools: in.Tools, AllowedHosts: in.AllowedHosts, DatabaseConnection: in.DatabaseConnection, MaxIterations: int32(in.MaxIterations),
		MaxTokens: pgInt(in.MaxTokens), Temperature: pgFloat(in.Temperature), UpdatedBy: pgtype.Text{String: actor, Valid: actor != ""},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, exception.Conflict("name_taken", fmt.Sprintf("an agent named %q already exists", in.Name))
	}
	if err != nil {
		return Agent{}, fmt.Errorf("create agent: %w", err)
	}
	return fromRow(row, in.Connection), nil
}

// Update replaces an agent's definition and bumps its revision.
func (s *Service) Update(ctx context.Context, orgID pgtype.UUID, actor, name string, in Input) (Agent, error) {
	in.Name = name // the name is the identity; it cannot change
	connectionID, err := s.validate(ctx, orgID, &in)
	if err != nil {
		return Agent{}, err
	}
	row, err := s.q.UpdateAgent(ctx, gen.UpdateAgentParams{
		OrgID: orgID, Name: name, Description: in.Description, ConnectionID: connectionID, Model: in.Model,
		SystemPrompt: in.System, InputSchema: nonNull(in.InputSchema), OutputSchema: nonNull(in.OutputSchema),
		Tools: in.Tools, AllowedHosts: in.AllowedHosts, DatabaseConnection: in.DatabaseConnection, MaxIterations: int32(in.MaxIterations),
		MaxTokens: pgInt(in.MaxTokens), Temperature: pgFloat(in.Temperature), UpdatedBy: pgtype.Text{String: actor, Valid: actor != ""},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, exception.NotFound("agent " + name)
	}
	if err != nil {
		return Agent{}, fmt.Errorf("update agent: %w", err)
	}
	return fromRow(row, in.Connection), nil
}

func pgInt(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true}
}

func pgFloat(v *float64) pgtype.Float8 {
	if v == nil {
		return pgtype.Float8{}
	}
	return pgtype.Float8{Float64: *v, Valid: true}
}

// Get returns one agent.
func (s *Service) Get(ctx context.Context, orgID pgtype.UUID, name string) (Agent, error) {
	row, err := s.q.GetAgentByName(ctx, gen.GetAgentByNameParams{OrgID: orgID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, exception.NotFound("agent " + name)
	}
	if err != nil {
		return Agent{}, fmt.Errorf("get agent: %w", err)
	}
	return fromRow(row.Agent, row.ConnectionName), nil
}

// List returns the organization's agents.
func (s *Service) List(ctx context.Context, orgID pgtype.UUID) ([]Agent, error) {
	rows, err := s.q.ListAgents(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	out := make([]Agent, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromRow(row.Agent, row.ConnectionName))
	}
	return out, nil
}

// Delete removes an agent. Workflows that still run it fail with E_AGENT_NOT_FOUND.
func (s *Service) Delete(ctx context.Context, orgID pgtype.UUID, name string) error {
	rows, err := s.q.DeleteAgent(ctx, gen.DeleteAgentParams{OrgID: orgID, Name: name})
	if err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	if rows == 0 {
		return exception.NotFound("agent " + name)
	}
	return nil
}

// Definition implements agent.Definitions: the executor's view of an agent.
func (s *Service) Definition(ctx context.Context, tenantID uuid.UUID, name string) (agent.Definition, error) {
	orgID := pgtype.UUID{Bytes: [16]byte(tenantID), Valid: true}
	row, err := s.q.GetAgentByName(ctx, gen.GetAgentByNameParams{OrgID: orgID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return agent.Definition{}, agent.ErrAgentNotFound
	}
	if err != nil {
		return agent.Definition{}, fmt.Errorf("load agent: %w", err)
	}
	a := row.Agent
	def := agent.Definition{
		Name: a.Name, Revision: int(a.Revision), Connection: row.ConnectionName, Model: a.Model, System: a.SystemPrompt,
		InputSchema: a.InputSchema, OutputSchema: a.OutputSchema, Tools: a.Tools, AllowedHosts: a.AllowedHosts, DatabaseConnection: a.DatabaseConnection,
		MaxIterations: int(a.MaxIterations), Temperature: floatPtr(a.Temperature),
	}
	if a.MaxTokens.Valid {
		def.MaxTokens = int(a.MaxTokens.Int32)
	}
	return def, nil
}

// ToolInfo describes a tool an agent may be given.
type ToolInfo struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Tools lists the registered tools, for the editor.
func (s *Service) Tools() []ToolInfo {
	names := s.tools.Names()
	out := make([]ToolInfo, 0, len(names))
	for _, name := range names {
		if tool, ok := s.tools.Get(name); ok {
			out = append(out, ToolInfo{Name: name, Description: tool.Description(), InputSchema: tool.InputSchema()})
		}
	}
	return out
}

// TestResult is the outcome of running an agent once, outside any workflow.
type TestResult struct {
	OK      bool
	Output  json.RawMessage
	Metrics map[string]any
	Code    string // set when !OK
	Message string
}

// Test runs the agent once with the given input (and optional prompt), as a
// workflow node would, and reports the result and what it cost. It spends real
// tokens but does not create a run.
func (s *Service) Test(ctx context.Context, tenantID pgtype.UUID, name string, input, prompt json.RawMessage) (TestResult, error) {
	if s.runner == nil {
		return TestResult{}, errors.New("agents: the executor is not attached")
	}
	if _, err := s.Get(ctx, tenantID, name); err != nil {
		return TestResult{}, err
	}
	config := map[string]json.RawMessage{"agent": mustJSON(name)}
	if len(input) > 0 && string(input) != "null" {
		config["input"] = input
	}
	if len(prompt) > 0 && string(prompt) != "null" {
		config["prompt"] = prompt
	}
	if _, hasInput := config["input"]; !hasInput {
		if _, hasPrompt := config["prompt"]; !hasPrompt {
			config["input"] = json.RawMessage(`{}`)
		}
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return TestResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	result, execErr := s.runner.Execute(ctx, executor.Request{
		TenantID: uuid.UUID(tenantID.Bytes), RunID: uuid.New(), NodeID: "agent_test", Config: raw, IdempotencyKey: "agent-test",
	})
	out := TestResult{Output: result.Output, Metrics: result.Metrics}
	if execErr != nil {
		out.Code, out.Message = "agent_failed", execErr.Error()
		var permanent *executor.PermanentError
		var retryable *executor.RetryableError
		switch {
		case errors.As(execErr, &permanent):
			out.Code = permanent.Code
		case errors.As(execErr, &retryable):
			out.Code = retryable.Code
		}
		return out, nil
	}
	out.OK = true
	return out, nil
}

func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
