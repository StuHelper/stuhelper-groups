// Package groups owns group admission, management and voluntary entertainment.
package groups

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/StuHelper/stuhelper-platform/account"
	"github.com/StuHelper/stuhelper-platform/admin"
	"time"
)

var (
	ErrInvalid         = errors.New("invalid_request")
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
	ErrNotFound        = errors.New("not_found")
	ErrConflict        = errors.New("conflict")
	ErrUnavailable     = errors.New("unavailable")
	ErrDisabled        = errors.New("disabled")
	ErrCooldown        = errors.New("cooldown")
)

type Config struct {
	Welcome          string   `json:"welcome"`
	RulesTemplate    string   `json:"rules_template"`
	CheckCard        bool     `json:"check_card"`
	RemindCard       bool     `json:"remind_card"`
	BlockLinks       bool     `json:"block_links"`
	FrequencySeconds int64    `json:"frequency_seconds"`
	FrequencyLimit   int64    `json:"frequency_limit"`
	WarningLimit     int64    `json:"warning_limit"`
	ID               string   `json:"id"`
	School           string   `json:"school"`
	Mode             string   `json:"mode"`
	RequireStudent   bool     `json:"require_student"`
	AcceptAlumni     bool     `json:"accept_alumni"`
	AcceptTemporary  bool     `json:"accept_temporary"`
	GraceSeconds     int64    `json:"grace_seconds"`
	ReminderSeconds  int64    `json:"reminder_seconds"`
	RetentionSeconds int64    `json:"retention_seconds"`
	PasswordHash     string   `json:"password_hash"`
	CardTemplate     string   `json:"card_template"`
	AutoCard         bool     `json:"auto_card"`
	DiceSides        int64    `json:"dice_sides"`
	MuteProbability  int64    `json:"mute_probability"`
	MuteSeconds      int64    `json:"mute_seconds"`
	CooldownSeconds  int64    `json:"cooldown_seconds"`
	Pity             int64    `json:"pity"`
	Keywords         []string `json:"keywords"`
	RepeatThreshold  int64    `json:"repeat_threshold"`
	AutoActions      []string `json:"auto_actions"`
}
type Command struct {
	Kind     string `json:"kind"`
	Target   string `json:"target,omitempty"`
	Text     string `json:"text,omitempty"`
	Seconds  int64  `json:"seconds,omitempty"`
	Message  string `json:"message,omitempty"`
	Receiver string `json:"receiver,omitempty"`
	Enabled  bool   `json:"enabled,omitempty"`
}
type Result struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Action string `json:"action"`
	Value  int64  `json:"value"`
	Detail string `json:"detail"`
}
type Member struct {
	Joined     bool      `json:"joined"`
	Owner      string    `json:"owner"`
	State      string    `json:"state"`
	Deadline   time.Time `json:"deadline"`
	Reminder   time.Time `json:"reminder"`
	Password   bool      `json:"password"`
	Exempt     bool      `json:"exempt"`
	Extended   bool      `json:"extended"`
	Failures   int64     `json:"failures"`
	LastPlay   time.Time `json:"last_play"`
	MutedUntil time.Time `json:"muted_until"`
}
type Authorizer interface {
	Evaluate(context.Context, account.Principal, string, admin.Scope) error
}
type Action struct {
	ID         string            `json:"id"`
	Group      string            `json:"group"`
	Actor      account.Principal `json:"actor"`
	Capability string            `json:"capability"`
	Command    Command           `json:"command"`
	State      string            `json:"state"`
	Result     string            `json:"result"`
	Created    time.Time         `json:"created"`
}

// Bot must deduplicate by Action.ID and query actual outcomes after timeouts.
// Lookup returns found=false only when no execution took place. Never guess success.
type Outcome struct {
	Success bool
	Code    string
}
type Bot interface {
	Execute(context.Context, Action) (Outcome, error)
	Lookup(context.Context, string) (Outcome, bool, error)
}
type Analyzer interface {
	Analyze(context.Context, string) (string, []string, error)
}
type Event struct {
	SpecVersion string          `json:"specversion"`
	ID          string          `json:"id"`
	Source      string          `json:"source"`
	Type        string          `json:"type"`
	Subject     string          `json:"subject"`
	Time        time.Time       `json:"time"`
	Revision    int64           `json:"revision"`
	Data        json.RawMessage `json:"data"`
}
type Row struct {
	ID   string
	Data json.RawMessage
}
type Tx interface {
	Get(string, string, any) error
	Put(string, string, any) error
	Delete(string, string) error
	List(string, string, int) ([]Row, error)
	PendingActions(string) (int, error)
	Emit(Event) error
	Accept(Event) (bool, error)
}
type Repository interface {
	Transaction(context.Context, func(Tx) error) error
}
type Service struct {
	PlatformIdentity admin.Identity
	Repo             Repository
	Auth             Authorizer
	Facts            FactsProvider
	FactsPrincipal   account.Principal
	Bot              Bot
	AI               Analyzer
	Now              func() time.Time
	Random           func(int64) (int64, error)
}
type Page struct {
	Items      []json.RawMessage `json:"items"`
	NextCursor string            `json:"next_cursor"`
}
