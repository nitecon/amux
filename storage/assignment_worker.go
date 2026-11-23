package storage

import (
	"context"
	"errors"
	"time"

	"github.com/nitecon/amux/eventbus"
)

// RunAssignmentWorker consumes the shared channel bus. Only ticks assign work;
// presence and task notifications never bypass the scheduling boundary.
func (s *Store) RunAssignmentWorker(ctx context.Context) error {
	return s.runAssignmentWorker(ctx, nil)
}

func (s *Store) runAssignmentWorker(ctx context.Context, ready chan<- struct{}) error {
	for {
		sub := s.bus.Subscribe(128)
		if err := s.ReconcilePresence(ctx, time.Now().UTC()); err != nil {
			sub.Unsubscribe()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if ready != nil {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
		for {
			select {
			case <-ctx.Done():
				sub.Unsubscribe()
				return nil
			case event, ok := <-sub.C:
				if !ok {
					err := sub.Err()
					sub.Unsubscribe()
					if ctx.Err() != nil {
						return nil
					}
					if !errors.Is(err, eventbus.ErrSlowConsumer) {
						return err
					}
					// Reconcile committed presence after overflow, then wait for
					// the next tick before allocating any task.
					break
				}
				if event.Topic == eventbus.Tick {
					if err := s.ReconcileAssignments(ctx, time.Now().UTC()); err != nil {
						sub.Unsubscribe()
						if ctx.Err() != nil {
							return nil
						}
						return err
					}
				}
				continue
			}
			break
		}
	}
}
