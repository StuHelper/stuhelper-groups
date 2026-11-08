package groups

import (
	"context"

	"github.com/StuHelper/stuhelper-platform/account"
)

// CardField separates disclosure permission from source availability. Only
// allowed and available fields may enter a group card; absence is not a violation.
type CardField struct {
	Allowed   bool
	Available bool
	Value     string
}

// Facts contains current eligibility and card disclosure decisions supplied by backend.
// Bound is true only when all three independent QQ bindings are currently complete.
type Facts struct {
	StudentKnown bool
	StageKnown   bool
	Bound        bool
	Student      bool
	Alumni       bool
	Temporary    bool
	Fields       map[string]CardField
}

// FactsProvider is owned by groups; backend implements all provider mappings.
// Unknown stages and provider failures must not be turned into valid eligibility.
type FactsProvider interface {
	GroupFacts(ctx context.Context, principal account.Principal, owner, school string) (Facts, error)
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
