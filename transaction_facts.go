package groups

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"
)

var errPrepareTransaction = errors.New("refresh group transaction inputs")

type observedFacts struct {
	value      Facts
	generation string
}

// A write pass discovers its group locks and required facts using the same checks
// as the final operation. An incomplete pass rolls back, including tentative inbox writes.
type preparedTx struct {
	Tx
	held        map[string]bool
	extra       map[string]bool
	cached      map[factKey]observedFacts
	needed      map[factKey]string
	generations map[string]string
}

func (s *Service) runPrepared(ctx context.Context, scopes []string, fn func(*preparedTx) error) error {
	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	held := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		held[scope] = true
	}
	cached := make(map[factKey]observedFacts)
	for call.Err() == nil {
		pass := &preparedTx{held: held, extra: make(map[string]bool), cached: cached, needed: make(map[factKey]string), generations: make(map[string]string)}
		err := s.runScoped(call, slices.Collect(maps.Keys(held)), func(tx Tx) error {
			pass.Tx = tx
			return fn(pass)
		})
		if !errors.Is(err, errPrepareTransaction) {
			return err
		}
		for scope := range pass.extra {
			held[scope] = true
		}
		for key, gen := range pass.needed {
			facts, err := s.currentFacts(call, key.owner, key.school)
			if err != nil {
				return ErrUnavailable
			}
			cached[key] = observedFacts{facts, gen}
		}
	}
	return call.Err()
}

func (t *preparedTx) requireScope(scope string) {
	if !t.held[scope] {
		t.extra[scope] = true
	}
}
func (t *preparedTx) prepareFacts(keys []factKey) error {
	for _, key := range keys {
		t.requireScope("owner:" + key.owner)
		generation, seen := t.generations[key.owner]
		if !seen {
			var err error
			generation, err = eligibilityGeneration(t, key.owner)
			if err != nil {
				return err
			}
			t.generations[key.owner] = generation
		}
		if v, ok := t.cached[key]; !ok || v.generation != generation {
			t.needed[key] = generation
		}
	}
	if len(t.extra) > 0 || len(t.needed) > 0 {
		return errPrepareTransaction
	}
	return nil
}

func (s *Service) transactionFacts(ctx context.Context, tx ReadTx, key factKey) (Facts, error) {
	if t, ok := tx.(*preparedTx); ok {
		if err := t.prepareFacts([]factKey{key}); err != nil {
			return Facts{}, err
		}
		return t.cached[key].value, nil
	}
	facts, err := s.currentFacts(ctx, key.owner, key.school)
	if err != nil {
		if preview, ok := tx.(*previewTx); ok {
			preview.factsFailed = true
		}
		return Facts{}, ErrUnavailable
	}
	return facts, nil
}

func eligibilityGeneration(tx ReadTx, owner string) (string, error) {
	var generation string
	err := tx.Get("eligibility_generations", owner, &generation)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return generation, err
}
