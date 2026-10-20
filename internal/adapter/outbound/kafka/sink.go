// Package kafka là adapter gửi sự kiện từ outbox lên Kafka (hoặc broker tương thích giao thức
// Kafka: Redpanda, MSK, Confluent, Event Hubs) qua franz-go. Adapter không biết schema của sự
// kiện: payload và header được chuyển nguyên vẹn.
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

// deliveryTimeout giới hạn thời gian franz-go tự thử lại một record: broker sập thì Publish
// trả lỗi (relay thử lại lô sau) thay vì treo và giữ khoá outbox mãi.
const deliveryTimeout = 20 * time.Second

// Sink hiện thực outbox.Sink.
type Sink struct {
	client *kgo.Client
}

var _ outbox.Sink = (*Sink)(nil)

// Option tuỳ chỉnh kết nối tới broker.
type Option func(*options) error

type options struct {
	tls  *tls.Config
	sasl sasl.Mechanism
}

// WithTLS mã hoá kết nối bằng cfg (xem TLSConfig).
func WithTLS(cfg *tls.Config) Option {
	return func(o *options) error { o.tls = cfg; return nil }
}

// WithSASL xác thực với broker. mechanism: PLAIN, SCRAM-SHA-256 hoặc SCRAM-SHA-512 (Amazon MSK,
// Confluent Cloud, Aiven... đều hỗ trợ ít nhất một cơ chế SCRAM hoặc PLAIN qua TLS).
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

// TLSConfig tạo cấu hình TLS cho client: TLS 1.2 trở lên, tin CA của hệ thống, hoặc chỉ CA trong
// caFile (PEM) nếu được chỉ định. Không có tuỳ chọn bỏ qua kiểm tra chứng chỉ.
func TLSConfig(caFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile == "" {
		return cfg, nil
	}
	pemBytes, err := os.ReadFile(filepath.Clean(caFile)) // đường dẫn do người vận hành cấu hình
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

// NewSink tạo producer gửi vào topic. franz-go mặc định bật idempotent producer và acks=all:
// broker không ghi trùng khi producer thử lại, và thứ tự trong một partition được giữ.
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

// Ping kiểm tra kết nối tới broker; dùng lúc khởi động để cấu hình sai lộ ra ngay.
func (s *Sink) Ping(ctx context.Context) error {
	if err := s.client.Ping(ctx); err != nil {
		return fmt.Errorf("kafka ping: %w", err)
	}
	return nil
}

// Publish gửi cả lô và chờ broker trả kết quả cho từng record. Key là ID của aggregate, nên mọi
// sự kiện của một user nằm trên cùng partition, theo đúng thứ tự trong lô. Record bị broker từ
// chối vì nội dung được trả lỗi bọc outbox.ErrPermanent; lỗi khác (mạng, leader đổi, topic chưa
// có, thiếu quyền) là tạm thời và sẽ được thử lại.
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
		sort.Strings(keys) // thứ tự header ổn định giữa các lần gửi
		for _, k := range keys {
			headers = append(headers, kgo.RecordHeader{Key: k, Value: []byte(m.Headers[k])})
		}
		records[i] = &kgo.Record{Key: []byte(m.Key), Value: m.Payload, Headers: headers}
		index[records[i]] = i
	}
	errs := make([]error, len(msgs))
	var rejected []int
	// ProduceSync trả kết quả theo thứ tự hoàn thành, không theo thứ tự gửi: ghép lại bằng record.
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
	// Broker áp giới hạn kích thước lên cả batch, nên một record quá lớn kéo theo mọi record đi
	// chung batch với nó. Gửi lại riêng từng record bị từ chối: chỉ record vẫn bị từ chối khi đứng
	// một mình mới là lỗi vĩnh viễn.
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

// isPermanent: broker từ chối chính record này; gửi lại y nguyên cũng sẽ bị từ chối.
func isPermanent(err error) bool {
	return errors.Is(err, kerr.MessageTooLarge) || errors.Is(err, kerr.RecordListTooLarge) ||
		errors.Is(err, kerr.InvalidRecord) || errors.Is(err, kerr.CorruptMessage)
}

// Close đóng kết nối tới broker.
func (s *Sink) Close() { s.client.Close() }
