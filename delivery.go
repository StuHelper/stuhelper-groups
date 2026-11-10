package groups

import "context"

const OutboxChannel = "groups_outbox"

// OutboxReader is a trusted backend relay port. Exclusions are applied in SQL
// before ordering and limiting. Confirm only after all consumers commit durably.
type OutboxReader interface {
	PendingOutbox(ctx context.Context, limit int, excludeSubjects []string) ([]Event, error)
	ConfirmOutbox(ctx context.Context, id string) error
}
