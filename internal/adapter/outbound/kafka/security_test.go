package kafka_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"

	"github.com/example/userapi/internal/adapter/outbound/kafka"
	"github.com/example/userapi/internal/outbox"
)

func publishOne(t *testing.T, sink *kafka.Sink) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sink.Ping(ctx); err != nil {
		return err
	}
	return sink.Publish(ctx, []outbox.Message{{ID: "e1", Key: "u1", Type: "user.registered", Payload: []byte(`{}`)}})[0]
}

// Brokers requiring SASL (MSK, Confluent, Aiven...): authenticate using SCRAM or PLAIN.
func TestSink_AuthenticatesWithSASL(t *testing.T) {
	for _, mech := range []string{"SCRAM-SHA-256", "SCRAM-SHA-512", "PLAIN"} {
		t.Run(mech, func(t *testing.T) {
			c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "users"),
				kfake.EnableSASL(), kfake.Superuser(mech, "relay", "s3cret"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(c.Close)

			sink, err := kafka.NewSink(c.ListenAddrs(), "users", kafka.Options{SASLMechanism: mech, SASLUsername: "relay", SASLPassword: "s3cret"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(sink.Close)
			if err := publishOne(t, sink); err != nil {
				t.Fatalf("publish with valid credentials: %v", err)
			}

			wrong, err := kafka.NewSink(c.ListenAddrs(), "users", kafka.Options{SASLMechanism: mech, SASLUsername: "relay", SASLPassword: "wrong"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(wrong.Close)
			if err := publishOne(t, wrong); err == nil {
				t.Fatal("publish with a wrong password succeeded")
			}
		})
	}
}

func TestNewSink_RejectsUnknownSASLMechanism(t *testing.T) {
	if _, err := kafka.NewSink([]string{"127.0.0.1:1"}, "users", kafka.Options{SASLMechanism: "GSSAPI", SASLUsername: "u", SASLPassword: "p"}); err == nil {
		t.Fatal("unknown SASL mechanism accepted")
	}
}

// TLS with a private CA: trust the specified CA file and reject the broker without it;
// never disable certificate verification.
func TestSink_ConnectsOverTLSWithACAFile(t *testing.T) {
	serverCfg, caFile := selfSignedTLS(t)
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "users"), kfake.TLS(serverCfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	tlsCfg, err := kafka.TLSConfig(caFile)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := kafka.NewSink(c.ListenAddrs(), "users", kafka.Options{TLS: tlsCfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	if err := publishOne(t, sink); err != nil {
		t.Fatalf("publish over TLS: %v", err)
	}

	systemRoots, err := kafka.TLSConfig("") // Trust only system CAs: reject the self-signed CA.
	if err != nil {
		t.Fatal(err)
	}
	untrusted, err := kafka.NewSink(c.ListenAddrs(), "users", kafka.Options{TLS: systemRoots})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(untrusted.Close)
	if err := publishOne(t, untrusted); err == nil {
		t.Fatal("connected to a broker whose certificate is not trusted")
	}
}

func TestTLSConfig_RejectsUnreadableCAFile(t *testing.T) {
	if _, err := kafka.TLSConfig(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatal("missing CA file accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := kafka.TLSConfig(bad); err == nil {
		t.Fatal("CA file without certificates accepted")
	}
}

// selfSignedTLS creates a self-signed CA for 127.0.0.1 and returns broker TLS config and the CA file path.
func selfSignedTLS(t *testing.T) (*tls.Config, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kfake test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, caFile
}
