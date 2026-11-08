package adapters

import (
	"context"

	"github.com/StuHelper/stuhelper-platform/account"
	v "github.com/StuHelper/stuhelper-verification"
)

// CardField separates disclosure permission from source availability. Only
// allowed and available fields may enter a group card; absence is not a violation.
type CardField struct {
	Allowed   bool
	Available bool
	Value     string
}

// GroupDetails describes current stage and approved card fields for one school.
// StageKnown=false never implies an ordinary enrolled student.
type GroupDetails struct {
	StageKnown bool
	Alumni     bool
	Temporary  bool
	Fields     map[string]CardField
}

// Details is a consumer-owned port. Backend bridges provider-owned DTOs without
// making verification or profiles import groups. Principal is a trusted service
// identity with school-scoped facts and per-field disclosure authorization.
type Details interface {
	GroupFacts(ctx context.Context, principal v.Principal, owner, school string) (GroupDetails, error)
}

// BotScope identifies a registered adapter instance. Platform is onebot or
// official; connect is never a bot transport or evidence of a shared QQ identity.
type BotScope struct {
	Platform string
	BotID    string
}

type BotIdentity struct {
	Bot    BotScope
	UserID string
}

// IdentityResolver is the backend bot bridge's consumer-owned port. The provider
// must check the current service identity, bot scope and binding on every call.
// Errors use groups sentinels (errors.Is); unavailable is never a missing binding.
type IdentityResolver interface {
	ResolveOwner(ctx context.Context, principal account.Principal, identity BotIdentity) (string, error)
	ResolveBotIdentity(ctx context.Context, principal account.Principal, owner string, bot BotScope) (BotIdentity, error)
}
