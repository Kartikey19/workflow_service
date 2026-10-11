package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kartikey/workflow_service/internal/core/executor"
	"github.com/kartikey/workflow_service/pkg/mongox"
	"github.com/kartikey/workflow_service/pkg/netguard"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"net/url"
)

type Mongo struct {
	conns  executor.Connections
	policy netguard.Policy
	action Action
}

func MongoSettings(config map[string]any, secret map[string]string) mongox.Config {
	c := mongox.Config{URI: secret["uri"]}
	c.Database, _ = config["database"].(string)
	c.TLS, _ = config["tls"].(string)
	return c
}
func MongoNodes(conns executor.Connections, policy netguard.Policy) []executor.Executor {
	out := []executor.Executor{}
	for _, a := range ActionsFor("mongo") {
		out = append(out, &Mongo{conns, policy, a})
	}
	return out
}
func (m *Mongo) Type() string { return m.action.Name }
func (m *Mongo) Describe() executor.Description {
	return executor.Description{Title: m.action.Title, Category: "integration", Description: m.action.Title + " through a saved MongoDB connector. Finds are bounded and cannot run server-side JavaScript. Inserts may partially succeed or be repeated on retries.", Output: "Collections, documents, or inserted count"}
}
func (m *Mongo) ValidateConfig(raw json.RawMessage) error { return checkActionConfig(m.action, raw) }
func (m *Mongo) Execute(ctx context.Context, req executor.Request) (executor.Result, error) {
	c, args, err := decodeAction(m.action, req.Config)
	if err != nil {
		return executor.Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, mongox.Timeout)
	defer cancel()
	if m.conns == nil {
		return executor.Result{}, executor.Permanent("bad_config", errors.New("saved connections unavailable"))
	}
	conn, err := m.conns.Connection(ctx, req.TenantID, c, "mongo")
	if err != nil {
		if errors.Is(err, executor.ErrConnectionNotFound) {
			return executor.Result{}, executor.Permanent("E_CONNECTION_NOT_FOUND", err)
		}
		return executor.Result{}, executor.Retryable("connection_lookup", err)
	}
	req.RedactSecrets(conn.Secret["uri"])
	if u, e := url.Parse(conn.Secret["uri"]); e == nil && u.User != nil {
		password, _ := u.User.Password()
		req.RedactSecrets(password)
	}
	cfg := MongoSettings(conn.Config, conn.Secret)
	cfg.Dial = m.policy.DialContext
	db, err := mongox.Open(ctx, cfg)
	if err != nil {
		return executor.Result{}, mongoFailure(ctx, err)
	}
	defer db.Close()
	var in struct {
		Collection               string
		Filter, Projection, Sort json.RawMessage
		MaxRows                  int `json:"max_rows"`
		Documents                []json.RawMessage
	}
	raw, _ := json.Marshal(args)
	_ = json.Unmarshal(raw, &in)
	var output any
	switch m.Type() {
	case "mongo.list_collections":
		names, e := db.Collections(ctx)
		err = e
		output = map[string]any{"collections": names}
	case "mongo.find":
		output, err = db.Find(ctx, in.Collection, in.Filter, in.Projection, in.Sort, in.MaxRows)
	case "mongo.insert":
		var count int
		count, err = db.Insert(ctx, in.Collection, in.Documents)
		output = map[string]any{"inserted_count": count}
	}
	if err != nil {
		return executor.Result{}, mongoFailure(ctx, err)
	}
	raw, err = json.Marshal(output)
	return executor.Result{Output: raw}, err
}
func mongoFailure(ctx context.Context, err error) error {
	if errors.Is(err, netguard.ErrBlocked) {
		return executor.Permanent("egress_blocked", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if mongo.IsTimeout(err) || mongo.IsNetworkError(err) {
		return executor.Retryable("database_error", err)
	}
	return executor.Permanent("database_error", err)
}
