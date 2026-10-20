// Package kafka forwards outbox events to Kafka or compatible brokers
// (Redpanda, MSK, Confluent, Event Hubs) through franz-go. The adapter does not know event schemas:
// payloads and headers pass through unchanged.
package kafka

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/example/userapi/internal/outbox"
)

// deliveryTimeout bounds franz-go record retries: if the broker is down, Publish returns an error
// for a later relay retry instead of hanging while holding outbox locks indefinitely.
const deliveryTimeout = 20 * time.Second

// Sink implements outbox.Sink.
type Sink struct {
	client *kgo.Client
}

var _ outbox.Sink = (*Sink)(nil)

// Option configures the broker connection.
type Option func(*options) error

type options struct {
	tls  *tls.Config
	sasl sasl.Mechanism
}

// WithTLS encrypts connections using cfg (see TLSConfig).
func WithTLS(cfg *tls.Config) Option {
	return func(o *options) error { o.tls = cfg; return nil }
}

// WithSASL authenticates to the broker using PLAIN, SCRAM-SHA-256, or SCRAM-SHA-512 (Amazon MSK,
// Confluent Cloud, Aiven... support at least one SCRAM mechanism or PLAIN over TLS).
func WithSASL(mechanism, username, password string) Option {
	return func(o *options) error {
		switch strings.ToUpper(mechanism) {
		case "PLAIN":
			o.sasl = plain.Auth{User: username, Pass: password}.AsMechanism()
		case "SCRAM-SHA-256":
			o.sasl = scram.Auth{User: username, Pass: password}.AsSha256Mechanism()
		case "SCRAM-SHA-512":
			o.sasl = scram.Auth{User: username, Pass: password}.AsSha512Mechanism()
		default:
			return fmt.Errorf("unsupported SASL mechanism %q", mechanism)
		}
		return nil
	}
}

// TLSConfig creates a TLS 1.2+ client config trusting system CAs, or only CAs in
// caFile (PEM) when specified. Certificate verification cannot be disabled.
func TLSConfig(caFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile == "" {
		return cfg, nil
	}
	pemBytes, err := os.ReadFile(filepath.Clean(caFile)) // The operator configures this path.
	if err != nil {
		return nil, fmt.Errorf("kafka CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("kafka CA file %s: no PEM certificates found", caFile)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// NewSink creates a topic producer. franz-go enables idempotent production and acks=all by default:
// producer retries do not duplicate broker records, and partition ordering is preserved.
func NewSink(brokers []string, topic string, opts ...Option) (*Sink, error) {
	var o options
	for _, opt := range opts {
		if err := opt(&o); err != nil {
			return nil, err
		}
	}
	kopts := []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.DefaultProduceTopic(topic),
		kgo.RecordDeliveryTimeout(deliveryTimeout),
	}
	if o.tls != nil {
		kopts = append(kopts, kgo.DialTLSConfig(o.tls))
	}
	if o.sasl != nil {
		kopts = append(kopts, kgo.SASL(o.sasl))
	}
	client, err := kgo.NewClient(kopts...)
	if err != nil {
		return nil, fmt.Errorf("kafka client: %w", err)
	}
	return &Sink{client: client}, nil
}

// Ping checks broker connectivity at startup to expose configuration errors early.
func (s *Sink) Ping(ctx context.Context) error {
	if err := s.client.Ping(ctx); err != nil {
		return fmt.Errorf("kafka ping: %w", err)
	}
	return nil
}

// Publish sends a batch and waits for each record result. The aggregate ID is the key, placing
// a user's events in one partition in batch order. Content-based broker rejections wrap
// outbox.ErrPermanent; other failures (network, leader changes, missing topics,
// missing permissions) are temporary and retried.
func (s *Sink) Publish(ctx context.Context, msgs []outbox.Message) []error {
	records := make([]*kgo.Record, len(msgs))
	index := make(map[*kgo.Record]int, len(msgs))
	for i, m := range msgs {
		headers := []kgo.RecordHeader{
			{Key: "event_id", Value: []byte(m.ID)},
			{Key: "event_type", Value: []byte(m.Type)},
		}
		keys := make([]string, 0, len(m.Headers))
		for k := range m.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys) // Keep header order stable across sends.
		for _, k := range keys {
			headers = append(headers, kgo.RecordHeader{Key: k, Value: []byte(m.Headers[k])})
		}
		records[i] = &kgo.Record{Key: []byte(m.Key), Value: m.Payload, Headers: headers}
		index[records[i]] = i
	}
	errs := make([]error, len(msgs))
	var rejected []int
	// ProduceSync returns completion order, not send order: match results by record.
	for _, r := range s.client.ProduceSync(ctx, records...) {
		if r.Err == nil {
			continue
		}
		i := index[r.Record]
		errs[i] = fmt.Errorf("kafka produce: %w", r.Err)
		if isPermanent(r.Err) {
			rejected = append(rejected, i)
		}
	}
	// Broker size limits apply to batches, so one oversized record can reject every record in its
	// batch. Retry rejected records individually: only a record still rejected on its own
	// is a permanent failure.
	for _, i := range rejected {
		retry := &kgo.Record{Key: records[i].Key, Value: records[i].Value, Headers: records[i].Headers}
		switch err := s.client.ProduceSync(ctx, retry).FirstErr(); {
		case err == nil:
			errs[i] = nil
		case isPermanent(err):
			errs[i] = fmt.Errorf("%w: kafka produce: %w", outbox.ErrPermanent, err)
		default:
			errs[i] = fmt.Errorf("kafka produce: %w", err)
		}
	}
	return errs
}

// isPermanent means this record was rejected and an unchanged retry would also fail.
func isPermanent(err error) bool {
	return errors.Is(err, kerr.MessageTooLarge) || errors.Is(err, kerr.RecordListTooLarge) ||
		errors.Is(err, kerr.InvalidRecord) || errors.Is(err, kerr.CorruptMessage)
}

// Close closes the broker connection.
func (s *Sink) Close() { s.client.Close() }
