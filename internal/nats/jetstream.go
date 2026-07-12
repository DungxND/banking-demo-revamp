// Package nats — JetStream helpers for the BANKING_EVENTS durable event bus.
//
// This file is the single place that owns:
//   - The stream name and subject hierarchy.
//   - Stream creation (idempotent — safe to call on every boot).
//   - Idempotent publish with Nats-Msg-Id deduplication header.
//   - Durable pull consumer creation config for the balance projection.
//
// Nothing in this file blocks; all I/O is context-scoped.
// Callers retain the *nats.Conn so they can use it for both micro RPC and JetStream.
package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	iredis "banking-demo/internal/redis"
)

// JetStream stream / subject constants.
// All code that touches BANKING_EVENTS references these — never bare strings.
const (
	// StreamName is the durable JetStream stream that persists all banking domain events.
	StreamName = "BANKING_EVENTS"

	// SubjectTransferCompleted is published by transfer-service after every committed transfer.
	// JetStream consumers filter on this subject for balance projection and audit replay.
	SubjectTransferCompleted = "banking.events.transfer.completed"

	// ConsumerBalanceProjection is the durable consumer name used by account-service
	// to maintain the Redis "balance" hash. Multiple account-service replicas bind to the
	// same durable name — JetStream delivers each message to exactly one replica.
	ConsumerBalanceProjection = "account-service-balance"

	// streamDuplicateWindow is the window within which Nats-Msg-Id deduplication is active.
	// Must be longer than the maximum client retry interval so that a retried publish
	// with the same transfer ID is silently dropped by the server rather than double-applied.
	streamDuplicateWindow = 5 * time.Minute
)

// InitStream creates or updates the BANKING_EVENTS stream.
// The call is idempotent: if the stream already exists with identical config the
// server returns the existing stream; if config drifts the server updates it in place.
// Call once at service startup before publishing or consuming.
func InitStream(ctx context.Context, nc *nats.Conn) (jetstream.JetStream, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream.New: %w", err)
	}
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       StreamName,
		Subjects:   []string{"banking.events.>"},
		MaxAge:     30 * 24 * time.Hour, // 30-day retention
		Storage:    jetstream.FileStorage,
		Retention:  jetstream.LimitsPolicy,
		Replicas:   1,              // raise to 3 for HA clusters
		Duplicates: streamDuplicateWindow,
	})
	if err != nil {
		return nil, fmt.Errorf("create stream %s: %w", StreamName, err)
	}
	return js, nil
}

// PublishTransferEvent publishes a TransferCompleted event to JetStream with a
// deduplication header keyed on the transfer ID.
//
// The Nats-Msg-Id header causes the JetStream server to silently discard any
// subsequent publish with the same header value within streamDuplicateWindow —
// this is Gap-5 idempotency: if the HTTP caller retries and the transfer is
// already committed, the duplicate event is dropped before any consumer sees it.
//
// Non-fatal from the caller's perspective: the transfer is already committed to
// PostgreSQL. Callers should log the error and continue, not roll back.
func PublishTransferEvent(ctx context.Context, js jetstream.JetStream, evt iredis.TransferCompleted) error {
	data, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal transfer event: %w", err)
	}
	_, err = js.PublishMsg(ctx, &nats.Msg{
		Subject: SubjectTransferCompleted,
		Data:    data,
		Header: nats.Header{
			// Dedup key — server discards a second publish with the same ID
			// within the stream's DuplicateWindow (5 min).
			"Nats-Msg-Id": []string{strconv.Itoa(int(evt.TransferID))},
		},
	})
	if err != nil {
		return fmt.Errorf("publish transfer event: %w", err)
	}
	return nil
}

// NewBalanceConsumer creates (or resumes) the durable pull consumer used by
// account-service to project the "balance" Redis hash from BANKING_EVENTS.
//
// DeliverAllPolicy: on a cold start (Redis wiped, new deployment) the consumer
// replays the full stream from sequence 0 and rebuilds the hash. On subsequent
// restarts it resumes from the last ACK offset — no full replay needed.
//
// MaxAckPending=100 provides flow control: the server stops delivering new
// messages once 100 are outstanding, preventing a slow-consumer disconnect
// during a Redis hiccup.
func NewBalanceConsumer(ctx context.Context, js jetstream.JetStream) (jetstream.Consumer, error) {
	cons, err := js.CreateOrUpdateConsumer(ctx, StreamName, jetstream.ConsumerConfig{
		Durable:        ConsumerBalanceProjection,
		FilterSubject:  SubjectTransferCompleted,
		AckPolicy:      jetstream.AckExplicitPolicy,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		AckWait:        10 * time.Second,
		MaxAckPending:  100,
		MaxDeliver:     5, // give up after 5 delivery attempts; message stays in the stream but is not redelivered
		BackOff:        []time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second},
	})
	if err != nil {
		return nil, fmt.Errorf("create consumer %s: %w", ConsumerBalanceProjection, err)
	}
	return cons, nil
}
