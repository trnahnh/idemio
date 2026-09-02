package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
)

type Publisher interface {
	WriteMessages(ctx context.Context, messages ...kafka.Message) error
}

type Relay struct {
	pool      *pgxpool.Pool
	publisher Publisher
	batch     int
	logger    *slog.Logger
}

func New(pool *pgxpool.Pool, publisher Publisher, batch int, logger *slog.Logger) *Relay {
	return &Relay{pool: pool, publisher: publisher, batch: batch, logger: logger}
}

const selectUnpublished = `
	SELECT intent_id::text, emitted_at, agent_id, idempotency_key::text, resource_type,
	       resource_id, operation, operation_class::text, scope_selector, payload,
	       voided_at IS NOT NULL
	  FROM write_intents
	 WHERE published_at IS NULL
	 ORDER BY emitted_at
	 LIMIT $1`

// intent_id is unique, so matching emitted_at against the same batch prunes partitions
// without widening the set of rows the update can touch.
const markPublished = `
	UPDATE write_intents SET published_at = now()
	 WHERE intent_id = ANY($1::uuid[]) AND emitted_at = ANY($2::timestamptz[])`

type message struct {
	IntentID       string          `json:"intent_id"`
	AgentID        string          `json:"agent_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	ResourceType   string          `json:"resource_type"`
	ResourceID     string          `json:"resource_id"`
	Operation      string          `json:"operation"`
	OperationClass string          `json:"operation_class"`
	ScopeSelector  []string        `json:"scope_selector"`
	Payload        json.RawMessage `json:"payload"`
	Voided         bool            `json:"voided"`
	EmittedAt      time.Time       `json:"emitted_at"`
}

// Publish, then mark. A crash between the two republishes, which is why ADR-0001 makes
// publication at-least-once and consumers tolerate duplicates.
func (r *Relay) Publish(ctx context.Context) (int, error) {
	pending, err := r.unpublished(ctx)
	if err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}

	messages := make([]kafka.Message, 0, len(pending))
	ids := make([]string, 0, len(pending))
	times := make([]time.Time, 0, len(pending))

	for _, intent := range pending {
		body, err := json.Marshal(intent)
		if err != nil {
			return 0, fmt.Errorf("encode intent %s: %w", intent.IntentID, err)
		}
		messages = append(messages, kafka.Message{Key: []byte(intent.IntentID), Value: body})
		ids = append(ids, intent.IntentID)
		times = append(times, intent.EmittedAt)
	}

	if err := r.publisher.WriteMessages(ctx, messages...); err != nil {
		return 0, fmt.Errorf("publish intents: %w", err)
	}
	if _, err := r.pool.Exec(ctx, markPublished, ids, times); err != nil {
		return 0, fmt.Errorf("mark intents published: %w", err)
	}
	return len(messages), nil
}

func (r *Relay) unpublished(ctx context.Context) ([]message, error) {
	rows, err := r.pool.Query(ctx, selectUnpublished, r.batch)
	if err != nil {
		return nil, fmt.Errorf("read outbox: %w", err)
	}
	defer rows.Close()

	var pending []message
	for rows.Next() {
		var intent message
		var payload []byte
		if err := rows.Scan(&intent.IntentID, &intent.EmittedAt, &intent.AgentID,
			&intent.IdempotencyKey, &intent.ResourceType, &intent.ResourceID,
			&intent.Operation, &intent.OperationClass, &intent.ScopeSelector,
			&payload, &intent.Voided); err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		intent.Payload = json.RawMessage(payload)
		pending = append(pending, intent)
	}
	return pending, rows.Err()
}

func (r *Relay) Run(ctx context.Context, interval time.Duration, published func(int), failed func()) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			count, err := r.Publish(ctx)
			if err != nil {
				failed()
				r.logger.Error("relay cycle failed; the write path is unaffected", "error", err)
				continue
			}
			if count > 0 {
				published(count)
				r.logger.Info("published intents", "count", count)
			}
		}
	}
}
