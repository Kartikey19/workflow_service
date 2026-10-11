// Package mongox provides bounded MongoDB operations without workflow knowledge.
package mongox

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const MaxRows = 1000
const MaxBytes = 1 << 20
const Timeout = 30 * time.Second

type Config struct {
	URI, Database, TLS string
	Dial               func(context.Context, string, string) (net.Conn, error)
}
type dialer struct {
	dial func(context.Context, string, string) (net.Conn, error)
}

func (d dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return d.dial(ctx, network, addr)
}

func Validate(c Config) error {
	if len(c.URI) > 8192 || strings.ContainsAny(c.URI, "\r\n\x00") {
		return errors.New("uri is too long or contains a control character")
	}
	u, err := url.Parse(c.URI)
	if err != nil || (u.Scheme != "mongodb" && u.Scheme != "mongodb+srv") || u.Host == "" {
		return errors.New("uri must be a mongodb:// or mongodb+srv:// connection string")
	}
	if c.Database == "" || len(c.Database) > 128 || strings.ContainsAny(c.Database, "/\\. \"$\x00") {
		return errors.New("database must be a MongoDB database name")
	}
	if c.TLS != "" && c.TLS != "verify-full" && c.TLS != "disable" {
		return errors.New("tls must be verify-full or disable")
	}
	// Security options are controlled by the saved connector, never by the URI.
	for key := range u.Query() {
		switch strings.ToLower(key) {
		case "tls", "ssl", "tlsinsecure", "tlsallowinvalidcertificates", "tlsallowinvalidhostnames", "tlscafile", "tlscertificatekeyfile", "tlscertificatekeyfilepassword":
			return errors.New("set TLS using the connector's tls field; TLS options in uri are not allowed")
		}
	}
	return nil
}

type Conn struct {
	client *mongo.Client
	db     *mongo.Database
}

func Open(ctx context.Context, c Config) (*Conn, error) {
	if err := Validate(c); err != nil {
		return nil, err
	}
	opts := options.Client().ApplyURI(c.URI).SetServerSelectionTimeout(Timeout).SetConnectTimeout(10 * time.Second).SetTimeout(Timeout).SetMaxPoolSize(4)
	if c.TLS == "disable" {
		opts.SetTLSConfig(nil)
	} else {
		opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	var dialMu sync.Mutex
	var dialError error
	if c.Dial != nil {
		opts.SetDialer(dialer{func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := c.Dial(ctx, network, addr)
			if err != nil {
				dialMu.Lock()
				dialError = err
				dialMu.Unlock()
			}
			return conn, err
		}})
	}
	if err := opts.Validate(); err != nil {
		return nil, errors.New("invalid MongoDB connection options")
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, scrub(err, c.URI)
	}
	conn := &Conn{client, client.Database(c.Database)}
	if err := conn.Ping(ctx); err != nil {
		conn.Close()
		dialMu.Lock()
		blocked := dialError
		dialMu.Unlock()
		if blocked != nil {
			err = fmt.Errorf("MongoDB connection: %w", errors.Join(err, blocked))
		}
		return nil, scrub(err, c.URI)
	}
	return conn, nil
}
func (c *Conn) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.client.Disconnect(ctx)
}
func (c *Conn) Ping(ctx context.Context) error { return c.client.Ping(ctx, nil) }
func (c *Conn) Collections(ctx context.Context) ([]string, error) {
	cursor, err := c.db.ListCollections(ctx, bson.D{}, options.ListCollections().SetNameOnly(true).SetBatchSize(100))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	names := []string{}
	for cursor.Next(ctx) {
		if len(names) >= 500 {
			return nil, errors.New("more than 500 collections; narrow database access")
		}
		var row struct {
			Name string `bson:"name"`
		}
		if err := cursor.Decode(&row); err != nil {
			return nil, err
		}
		names = append(names, row.Name)
	}
	return names, cursor.Err()
}
func CollectionName(name string) error {
	if name == "" || len(name) > 255 || strings.ContainsAny(name, "\x00$") || strings.HasPrefix(name, "system.") {
		return errors.New("invalid collection name")
	}
	return nil
}
func document(raw json.RawMessage) (bson.D, error) {
	if len(raw) == 0 {
		return bson.D{}, nil
	}
	var doc bson.D
	if err := bson.UnmarshalExtJSON(raw, false, &doc); err != nil {
		return nil, errors.New("expected an object containing valid MongoDB Extended JSON")
	}
	return doc, nil
}

// Reject executable server-side JavaScript, including inside nested predicates.
func safeFilter(raw json.RawMessage) error {
	var v any
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	var walk func(any) error
	walk = func(v any) error {
		switch v := v.(type) {
		case map[string]any:
			for key, child := range v {
				if key == "$where" || key == "$function" || key == "$accumulator" {
					return errors.New("server-side JavaScript is not permitted")
				}
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range v {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(v)
}

type Result struct {
	Documents []json.RawMessage `json:"documents"`
	RowCount  int               `json:"row_count"`
	Truncated bool              `json:"truncated"`
}

func (c *Conn) Find(ctx context.Context, name string, filter, projection, sort json.RawMessage, limit int) (Result, error) {
	out := Result{Documents: []json.RawMessage{}}
	if err := CollectionName(name); err != nil {
		return out, err
	}
	if err := safeFilter(filter); err != nil {
		return out, err
	}
	if err := safeFilter(projection); err != nil {
		return out, err
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > MaxRows {
		return out, errors.New("max_rows must be between 1 and 1000")
	}
	f, err := document(filter)
	if err != nil {
		return out, err
	}
	p, err := document(projection)
	if err != nil {
		return out, err
	}
	s, err := document(sort)
	if err != nil {
		return out, err
	}
	opts := options.Find().SetLimit(int64(limit + 1)).SetBatchSize(int32(min(limit+1, 100))).SetProjection(p).SetSort(s)
	cursor, err := c.db.Collection(name).Find(ctx, f, opts)
	if err != nil {
		return out, err
	}
	defer cursor.Close(ctx)
	total := 0
	for cursor.Next(ctx) {
		if len(out.Documents) >= limit {
			out.Truncated = true
			break
		}
		raw, err := bson.MarshalExtJSON(cursor.Current, false, false)
		if err != nil {
			return out, err
		}
		if total+len(raw) > MaxBytes {
			out.Truncated = true
			break
		}
		total += len(raw)
		out.Documents = append(out.Documents, json.RawMessage(raw))
	}
	out.RowCount = len(out.Documents)
	return out, cursor.Err()
}
func (c *Conn) Insert(ctx context.Context, name string, rows []json.RawMessage) (int, error) {
	if err := CollectionName(name); err != nil {
		return 0, err
	}
	if len(rows) == 0 || len(rows) > MaxRows {
		return 0, errors.New("documents must contain between 1 and 1000 objects")
	}
	docs := make([]any, 0, len(rows))
	size := 0
	for _, row := range rows {
		size += len(row)
		if size > MaxBytes {
			return 0, errors.New("documents exceed 1 MiB")
		}
		doc, err := document(row)
		if err != nil {
			return 0, err
		}
		docs = append(docs, doc)
	}
	result, err := c.db.Collection(name).InsertMany(ctx, docs)
	if err != nil {
		return 0, err
	}
	return len(result.InsertedIDs), nil
}

// Redaction keeps the original error chain available for egress classification.
type redacted struct {
	error
	message string
}

func (e redacted) Error() string { return e.message }
func (e redacted) Unwrap() error { return e.error }
func scrub(err error, uri string) error {
	message := strings.ReplaceAll(err.Error(), uri, "***")
	if u, e := url.Parse(uri); e == nil && u.User != nil {
		if p, ok := u.User.Password(); ok && p != "" {
			message = strings.ReplaceAll(message, p, "***")
		}
	}
	return redacted{err, fmt.Sprintf("MongoDB: %s", message)}
}
