package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
	"golang.org/x/crypto/bcrypt"
)

// Invite describes a pending invitation.
type Invite struct {
	ID        pgtype.UUID
	Email     string
	Role      string
	OrgName   string // the new organization, for a platform invite
	ExpiresAt time.Time
	CreatedAt time.Time
}

const inviteTokenPrefix = "inv_"

func inviteView(i gen.Invite, orgName string) Invite {
	name := orgName
	if i.OrgName.Valid {
		name = i.OrgName.String
	}
	return Invite{ID: i.ID, Email: i.Email, Role: i.Role, OrgName: name, ExpiresAt: i.ExpiresAt.Time, CreatedAt: i.CreatedAt.Time}
}

// CreateInvite invites email into the actor's organization. The returned token
// is the secret in the invite link; it is not stored.
func (s *Service) CreateInvite(ctx context.Context, actor auth.Principal, email, role string) (Invite, string, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return Invite{}, "", err
	}
	if role == "" {
		role = auth.RoleEditor
	}
	if !auth.ValidRole(role) {
		return Invite{}, "", exception.BadRequest("role must be viewer, editor, admin or owner")
	}
	if !mayGrant(actor, role) {
		return Invite{}, "", exception.Forbidden("you cannot invite someone above your own role")
	}
	if user, err := s.q.GetUserByEmail(ctx, email); err == nil {
		if _, err := s.q.GetMembership(ctx, gen.GetMembershipParams{OrgID: actor.TenantID, UserID: user.ID}); err == nil {
			return Invite{}, "", exception.Conflict("already_member", email+" is already a member")
		}
	}
	token, hash, err := newToken(inviteTokenPrefix)
	if err != nil {
		return Invite{}, "", err
	}
	row, err := s.q.CreateInvite(ctx, gen.CreateInviteParams{
		ID: newID(), OrgID: actor.TenantID, Email: email, Role: role, TokenHash: hash,
		InvitedBy: nullableUser(actor), ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(s.cfg.InviteTTL), Valid: true},
	})
	if err != nil {
		return Invite{}, "", fmt.Errorf("create invite: %w", err)
	}
	return inviteView(row, ""), token, nil
}

// CreatePlatformInvite invites email to a new organization that is created when
// the invite is accepted; the invitee becomes its owner. Platform admins only.
func (s *Service) CreatePlatformInvite(ctx context.Context, actor auth.Principal, email, orgName string) (Invite, string, error) {
	if !actor.PlatformAdmin {
		return Invite{}, "", exception.Forbidden("only platform admins can invite a new organization")
	}
	email, err := normalizeEmail(email)
	if err != nil {
		return Invite{}, "", err
	}
	orgName = strings.TrimSpace(orgName)
	if orgName == "" || len(orgName) > 100 {
		return Invite{}, "", exception.BadRequest("organization_name is required and at most 100 characters")
	}
	token, hash, err := newToken(inviteTokenPrefix)
	if err != nil {
		return Invite{}, "", err
	}
	row, err := s.q.CreateInvite(ctx, gen.CreateInviteParams{
		ID: newID(), OrgName: pgtype.Text{String: orgName, Valid: true}, Email: email, Role: auth.RoleOwner, TokenHash: hash,
		InvitedBy: nullableUser(actor), ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(s.cfg.InviteTTL), Valid: true},
	})
	if err != nil {
		return Invite{}, "", fmt.Errorf("create invite: %w", err)
	}
	return inviteView(row, ""), token, nil
}

func nullableUser(actor auth.Principal) pgtype.UUID {
	if actor.UserID.Valid {
		return actor.UserID
	}
	return pgtype.UUID{}
}

// ListInvites returns the organization's pending invites.
func (s *Service) ListInvites(ctx context.Context, orgID pgtype.UUID) ([]Invite, error) {
	rows, err := s.q.ListPendingInvites(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	out := make([]Invite, 0, len(rows))
	for _, row := range rows {
		out = append(out, inviteView(row, ""))
	}
	return out, nil
}

// RevokeInvite withdraws a pending invite.
func (s *Service) RevokeInvite(ctx context.Context, orgID, id pgtype.UUID) error {
	rows, err := s.q.DeleteInvite(ctx, gen.DeleteInviteParams{ID: id, OrgID: orgID})
	if err != nil {
		return fmt.Errorf("revoke invite: %w", err)
	}
	if rows == 0 {
		return exception.NotFound("invite")
	}
	return nil
}

// invalidInvite is the one answer for unknown, used and expired tokens.
func invalidInvite() error {
	return exception.New(410, "invite_invalid", "Invite is no longer valid", "the invite link is invalid, expired or already used")
}

// PreviewInvite lets the accept page show who is being invited where.
func (s *Service) PreviewInvite(ctx context.Context, token string) (Invite, bool, error) {
	row, err := s.q.GetInviteByTokenHash(ctx, hashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invite{}, false, invalidInvite()
	}
	if err != nil {
		return Invite{}, false, fmt.Errorf("find invite: %w", err)
	}
	if row.AcceptedAt.Valid || !row.ExpiresAt.Time.After(time.Now()) {
		return Invite{}, false, invalidInvite()
	}
	_, getErr := s.q.GetUserByEmail(ctx, row.Email)
	invite := inviteView(gen.Invite{
		ID: row.ID, OrgID: row.OrgID, OrgName: row.OrgName, Email: row.Email, Role: row.Role,
		ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt,
	}, row.ExistingOrgName.String)
	return invite, getErr == nil, nil
}

// AcceptInvite adds the invitee to the organization, creating the account (or,
// for an email that already has one, requiring that account's password) and the
// organization for a platform invite. It signs them in.
func (s *Service) AcceptInvite(ctx context.Context, token, password, name string) (Session, error) {
	var session Session
	err := pgxTx(ctx, s.pool, func(q *gen.Queries) error {
		invite, err := q.LockInviteByTokenHash(ctx, hashToken(token))
		if errors.Is(err, pgx.ErrNoRows) {
			return invalidInvite()
		}
		if err != nil {
			return fmt.Errorf("lock invite: %w", err)
		}
		if invite.AcceptedAt.Valid || !invite.ExpiresAt.Time.After(time.Now()) {
			return invalidInvite()
		}
		user, err := q.GetUserByEmail(ctx, invite.Email)
		switch {
		case err == nil:
			if user.DisabledAt.Valid || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
				return exception.New(401, "invalid_credentials", "Invalid credentials",
					"an account with this email already exists; enter its password to accept the invite")
			}
		case errors.Is(err, pgx.ErrNoRows):
			if err := checkPassword(password); err != nil {
				return err
			}
			hash, err := s.hashPassword(password)
			if err != nil {
				return err
			}
			if user, err = q.CreateUser(ctx, gen.CreateUserParams{ID: newID(), Email: invite.Email, Name: strings.TrimSpace(name), PasswordHash: hash}); err != nil {
				return fmt.Errorf("create user: %w", err)
			}
		default:
			return fmt.Errorf("find user: %w", err)
		}
		orgID := invite.OrgID
		if invite.OrgName.Valid {
			org, err := s.createOrganization(ctx, q, invite.OrgName.String, "free")
			if err != nil {
				return err
			}
			orgID = org.ID
		}
		if err := q.AddMembership(ctx, gen.AddMembershipParams{OrgID: orgID, UserID: user.ID, Role: invite.Role}); err != nil {
			return fmt.Errorf("add membership: %w", err)
		}
		if err := q.MarkInviteAccepted(ctx, invite.ID); err != nil {
			return fmt.Errorf("mark invite accepted: %w", err)
		}
		session, err = s.startSession(ctx, q, user, orgID)
		return err
	})
	return session, err
}
