package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mk990/daywatch/internal/config"
)

// buildFrame mirrors Laravel\Nightwatch\Payload::pull().
func buildFrame(tokenHash, payload string) string {
	length := len("v1") + 1 + len(tokenHash) + 1 + len(payload)
	return fmt.Sprintf("%d:v1:%s:%s", length, tokenHash, payload)
}

func TestReadFrame(t *testing.T) {
	payload := `[{"t":"request","duration":1234}]`
	frame := buildFrame("abc1234", payload)

	got, tokenHash, err := readFrame(strings.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	if tokenHash != "abc1234" {
		t.Fatalf("tokenHash = %q", tokenHash)
	}
	if string(got) != payload {
		t.Fatalf("payload = %q", got)
	}
}

func TestReadFramePayloadWithColons(t *testing.T) {
	payload := `[{"t":"log","message":"a:b:c ::: more"}]`
	frame := buildFrame("deadbee", payload)
	got, _, err := readFrame(strings.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("payload = %q", got)
	}
}

func TestReadFrameRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "no-colons-here", ":::", "99999999999999:v1:x:y", "12:v2:abc:hello"} {
		if _, _, err := readFrame(strings.NewReader(in)); err == nil {
			t.Fatalf("expected error for %q", in)
		}
	}
}

func TestTokenHashMatchesPHP(t *testing.T) {
	// php -r 'echo substr(hash("xxh128", "my-secret-token"), 0, 7);' → c27c052
	if got := config.TokenHash("my-secret-token"); got != "c27c052" {
		t.Fatalf("TokenHash = %q, want c27c052", got)
	}
}

// memSink is written from connection goroutines and read by the test, so it
// guards its state.
type memSink struct {
	mu      sync.Mutex
	batches [][]json.RawMessage
	apps    []string
}

func (m *memSink) InsertRecords(_ context.Context, r []json.RawMessage, app string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batches = append(m.batches, r)
	m.apps = append(m.apps, app)
	return len(r), nil
}

// snapshot returns a copy of what has been stored so far.
func (m *memSink) snapshot() ([][]json.RawMessage, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.batches), slices.Clone(m.apps)
}

// waitFor blocks until at least n batches have been stored, or the deadline passes.
func (m *memSink) waitFor(n int, within time.Duration) [][]json.RawMessage {
	deadline := time.Now().Add(within)
	for {
		batches, _ := m.snapshot()
		if len(batches) >= n || time.Now().After(deadline) {
			return batches
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mapResolver backs the AppResolver interface with a static map for tests.
type mapResolver map[string]string

func (m mapResolver) ResolveApp(_ context.Context, hash string) (string, bool, bool, error) {
	name, ok := m[hash]
	return name, ok, len(m) > 0, nil
}

// slowSink simulates an insert that is still running when shutdown starts.
type slowSink struct {
	delay time.Duration
	mu    sync.Mutex
	rows  int
}

type failingSink struct{}

func (failingSink) InsertRecords(context.Context, []json.RawMessage, string) (int, error) {
	return 0, errors.New("database unavailable")
}

func (s *slowSink) InsertRecords(ctx context.Context, r []json.RawMessage, _ string) (int, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows += len(r)
	return len(r), nil
}

func (s *slowSink) stored() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows
}

// Shutdown must wait for an accepted frame to be stored before it is ACKed.
func TestServeDrainsInFlightInserts(t *testing.T) {
	sink := &slowSink{delay: 300 * time.Millisecond}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New("127.0.0.1:0", mapResolver{}, 2*time.Second, sink, log)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()

	conn, err := net.Dial("tcp", srv.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(buildFrame("c27c052", `[{"t":"request","duration":1}]`))); err != nil {
		t.Fatal(err)
	}
	// Shut down while the sink is still handling the accepted frame.
	time.Sleep(50 * time.Millisecond)
	cancel()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 4)); err != nil {
		t.Fatal("no ack after draining persisted frame:", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after context cancellation")
	}
	if got := sink.stored(); got != 1 {
		t.Fatalf("stored %d records after drain, want 1", got)
	}
}

func TestServerEndToEnd(t *testing.T) {
	sink := &memSink{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New("127.0.0.1:0", mapResolver{"c27c052": "shop", "beef123": "blog"}, 2*time.Second, sink, log)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve(t.Context())

	addr := srv.ln.Addr().String()

	send := func(frame string) string {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte(frame)); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		resp := make([]byte, 4)
		if _, err := io.ReadFull(conn, resp); err != nil {
			t.Fatal(err)
		}
		return string(resp)
	}

	if got := send(buildFrame("c27c052", "PING")); got != "2:OK" {
		t.Fatalf("PING ack = %q", got)
	}
	if got := send(buildFrame("c27c052", "[]")); got != "2:OK" {
		t.Fatalf("empty batch ack = %q", got)
	}
	if got := send(buildFrame("c27c052", "{invalid")); got != "2:OK" {
		t.Fatalf("invalid JSON ack = %q", got)
	}

	records := `[{"t":"request","timestamp":1752700000.5,"duration":1000},{"t":"query","timestamp":1752700000.6,"duration":50}]`
	if got := send(buildFrame("c27c052", records)); got != "2:OK" {
		t.Fatalf("records ack = %q", got)
	}

	batches := sink.waitFor(1, 2*time.Second)
	if len(batches) != 1 || len(batches[0]) != 2 {
		t.Fatalf("batches = %+v", batches)
	}
	if !bytes.Contains(batches[0][0], []byte(`"request"`)) {
		t.Fatalf("first record = %s", batches[0][0])
	}
	if _, apps := sink.snapshot(); apps[0] != "shop" {
		t.Fatalf("app = %q, want shop", apps[0])
	}

	// A second app's token routes to its own app name.
	if got := send(buildFrame("beef123", records)); got != "2:OK" {
		t.Fatalf("blog ack = %q", got)
	}
	sink.waitFor(2, 2*time.Second)
	if _, apps := sink.snapshot(); len(apps) != 2 || apps[1] != "blog" {
		t.Fatalf("apps = %v, want [shop blog]", apps)
	}

	// Wrong token: still ACKed (matching official agent), but not stored.
	send(buildFrame("badbad1", records))
	time.Sleep(100 * time.Millisecond)
	if batches, _ := sink.snapshot(); len(batches) != 2 {
		t.Fatal("record with bad token was stored")
	}
}

func TestServerDoesNotACKFailedInsert(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New("127.0.0.1:0", mapResolver{"c27c052": "shop"}, 2*time.Second, failingSink{}, log)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve(t.Context())

	conn, err := net.Dial("tcp", srv.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(buildFrame("c27c052", `[{"t":"request"}]`))); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 4)); err == nil {
		t.Fatal("failed insert was acknowledged")
	}
}
