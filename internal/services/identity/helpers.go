package identity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/db"
	"github.com/kartikey/workflow_service/pkg/uid"
)

func newID() pgtype.UUID { return uid.New() }

// pgxTx runs fn in a transaction with queries bound to it.
func pgxTx(ctx context.Context, pool *pgxpool.Pool, fn func(q *gen.Queries) error) error {
	return db.InTx(ctx, pool, func(tx pgx.Tx) error { return fn(gen.New(tx)) })
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugify makes a URL-safe lowercase name.
func slugify(name string) string {
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if slug == "" {
		slug = "org"
	}
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	return slug
}

// createOrganization inserts an organization with a unique slug, adding a
// numeric suffix when the name is taken. A taken slug returns no row rather than
// an error, so it does not abort the surrounding transaction.
func (s *Service) createOrganization(ctx context.Context, q *gen.Queries, name, plan string) (gen.Organization, error) {
	base := slugify(name)
	for attempt := 0; attempt < 20; attempt++ {
		slug := base
		if attempt > 0 {
			slug = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		org, err := q.CreateOrganization(ctx, gen.CreateOrganizationParams{ID: newID(), Name: name, Slug: slug, PlanID: plan})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return gen.Organization{}, fmt.Errorf("create organization: %w", err)
		}
		return org, nil
	}
	return gen.Organization{}, fmt.Errorf("could not find a free organization slug for %q", name)
}
