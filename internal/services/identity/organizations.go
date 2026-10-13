package identity

import (
	"context"
	"fmt"
	"strings"

	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
)

// CreateOrganization creates an independent tenant owned by the signed-in user.
// Membership in another organization does not grant access to the new tenant.
func (s *Service) CreateOrganization(ctx context.Context, actor auth.Principal, name string) (Membership, error) {
	if !actor.SessionID.Valid || !actor.UserID.Valid {
		return Membership{}, exception.Forbidden("only signed-in users can create organizations")
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return Membership{}, exception.BadRequest("name is required and at most 100 characters")
	}
	var membership Membership
	err := pgxTx(ctx, s.pool, func(q *gen.Queries) error {
		org, err := s.createOrganization(ctx, q, name, "free")
		if err != nil {
			return err
		}
		if err := q.AddMembership(ctx, gen.AddMembershipParams{OrgID: org.ID, UserID: actor.UserID, Role: auth.RoleOwner}); err != nil {
			return fmt.Errorf("add organization owner: %w", err)
		}
		membership = Membership{OrgID: org.ID, OrgName: org.Name, OrgSlug: org.Slug, Role: auth.RoleOwner}
		return nil
	})
	return membership, err
}
