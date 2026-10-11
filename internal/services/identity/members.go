package identity

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
)

// Member is a user in an organization.
type Member struct {
	UserID   pgtype.UUID
	Email    string
	Name     string
	Role     string
	JoinedAt time.Time
}

// ListMembers returns the organization's members.
func (s *Service) ListMembers(ctx context.Context, orgID pgtype.UUID) ([]Member, error) {
	rows, err := s.q.ListMembers(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	out := make([]Member, 0, len(rows))
	for _, row := range rows {
		out = append(out, Member{UserID: row.UserID, Email: row.Email, Name: row.Name, Role: row.Role, JoinedAt: row.JoinedAt.Time})
	}
	return out, nil
}

// mayGrant reports whether actor can hand out role: nobody can grant more than
// they hold.
func mayGrant(actor auth.Principal, role string) bool {
	return actor.Can(role)
}

// UpdateMemberRole changes a member's role. Admins manage members below their
// own rank; only owners create or change owners; the last owner stays.
func (s *Service) UpdateMemberRole(ctx context.Context, actor auth.Principal, userID pgtype.UUID, role string) error {
	if !auth.ValidRole(role) {
		return exception.BadRequest("role must be viewer, editor, admin or owner")
	}
	if !mayGrant(actor, role) {
		return exception.Forbidden("you cannot grant a role above your own")
	}
	return pgxTx(ctx, s.pool, func(q *gen.Queries) error {
		members, err := q.LockOrgMemberships(ctx, actor.TenantID)
		if err != nil {
			return fmt.Errorf("lock memberships: %w", err)
		}
		current, owners := "", 0
		for _, m := range members {
			if m.Role == auth.RoleOwner {
				owners++
			}
			if m.UserID == userID {
				current = m.Role
			}
		}
		if current == "" {
			return exception.NotFound("member")
		}
		if !auth.AtLeast(actor.Role, current) {
			return exception.Forbidden("you cannot change a member above your own role")
		}
		if current == auth.RoleOwner && role != auth.RoleOwner && owners == 1 {
			return exception.Conflict("last_owner", "an organization needs at least one owner")
		}
		if _, err := q.UpdateMemberRole(ctx, gen.UpdateMemberRoleParams{OrgID: actor.TenantID, UserID: userID, Role: role}); err != nil {
			return fmt.Errorf("update member role: %w", err)
		}
		return nil
	})
}

// RemoveMember removes a member, or lets a member leave. Their sessions in
// this organization stop working at once (the session lookup joins memberships).
func (s *Service) RemoveMember(ctx context.Context, actor auth.Principal, userID pgtype.UUID) error {
	self := actor.UserID == userID
	if !self && !actor.Can(auth.RoleAdmin) {
		return exception.Forbidden("only admins can remove other members")
	}
	return pgxTx(ctx, s.pool, func(q *gen.Queries) error {
		members, err := q.LockOrgMemberships(ctx, actor.TenantID)
		if err != nil {
			return fmt.Errorf("lock memberships: %w", err)
		}
		current, owners := "", 0
		for _, m := range members {
			if m.Role == auth.RoleOwner {
				owners++
			}
			if m.UserID == userID {
				current = m.Role
			}
		}
		if current == "" {
			return exception.NotFound("member")
		}
		if !self && !auth.AtLeast(actor.Role, current) {
			return exception.Forbidden("you cannot remove a member above your own role")
		}
		if current == auth.RoleOwner && owners == 1 {
			return exception.Conflict("last_owner", "an organization needs at least one owner")
		}
		if _, err := q.DeleteMembership(ctx, gen.DeleteMembershipParams{OrgID: actor.TenantID, UserID: userID}); err != nil {
			return fmt.Errorf("remove member: %w", err)
		}
		return nil
	})
}

// APIKey is a key's metadata; the token itself is shown once, at creation.
type APIKey struct {
	ID         pgtype.UUID
	Name       string
	Prefix     string
	Role       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

const apiKeyPrefix = "wfk_"

// CreateAPIKey issues a key for the actor's organization. A key can hold at
// most the actor's own role, and never owner.
func (s *Service) CreateAPIKey(ctx context.Context, actor auth.Principal, name, role string) (APIKey, string, error) {
	if name == "" || len(name) > 100 {
		return APIKey{}, "", exception.BadRequest("name is required and at most 100 characters")
	}
	if role == "" {
		role = auth.RoleEditor
	}
	if role == auth.RoleOwner || !auth.ValidRole(role) {
		return APIKey{}, "", exception.BadRequest("role must be viewer, editor or admin")
	}
	if !mayGrant(actor, role) {
		return APIKey{}, "", exception.Forbidden("you cannot create a key above your own role")
	}
	token, hash, err := newToken(apiKeyPrefix)
	if err != nil {
		return APIKey{}, "", err
	}
	var createdBy pgtype.UUID
	if actor.UserID.Valid {
		createdBy = actor.UserID
	}
	row, err := s.q.CreateAPIKey(ctx, gen.CreateAPIKeyParams{
		ID: newID(), OrgID: actor.TenantID, Name: name, Prefix: token[:len(apiKeyPrefix)+6], TokenHash: hash, Role: role, CreatedBy: createdBy,
	})
	if err != nil {
		return APIKey{}, "", fmt.Errorf("create api key: %w", err)
	}
	return APIKey{ID: row.ID, Name: row.Name, Prefix: row.Prefix, Role: row.Role, CreatedAt: row.CreatedAt.Time}, token, nil
}

// ListAPIKeys returns the organization's active keys.
func (s *Service) ListAPIKeys(ctx context.Context, orgID pgtype.UUID) ([]APIKey, error) {
	rows, err := s.q.ListAPIKeys(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	out := make([]APIKey, 0, len(rows))
	for _, row := range rows {
		key := APIKey{ID: row.ID, Name: row.Name, Prefix: row.Prefix, Role: row.Role, CreatedAt: row.CreatedAt.Time}
		if row.LastUsedAt.Valid {
			t := row.LastUsedAt.Time
			key.LastUsedAt = &t
		}
		out = append(out, key)
	}
	return out, nil
}

// RevokeAPIKey disables a key immediately.
func (s *Service) RevokeAPIKey(ctx context.Context, orgID, id pgtype.UUID) error {
	rows, err := s.q.RevokeAPIKey(ctx, gen.RevokeAPIKeyParams{ID: id, OrgID: orgID})
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if rows == 0 {
		return exception.NotFound("api key")
	}
	return nil
}
