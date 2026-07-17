package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/mk/daywatch/internal/config"
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

type memSink struct {
	batches [][]json.RawMessage
	apps    []string
}

func (m *memSink) InsertRecords(_ context.Context, r []json.RawMessage, app string) (int, error) {
	m.batches = append(m.batches, r)
	m.apps = append(m.apps, app)
	return len(r), nil
}

func TestServerEndToEnd(t *testing.T) {
	sink := &memSink{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New("127.0.0.1:0", map[string]string{"c27c052": "shop", "beef123": "blog"}, 2*time.Second, sink, log)
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

	records := `[{"t":"request","timestamp":1752700000.5,"duration":1000},{"t":"query","timestamp":1752700000.6,"duration":50}]`
	if got := send(buildFrame("c27c052", records)); got != "2:OK" {
		t.Fatalf("records ack = %q", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for len(sink.batches) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(sink.batches) != 1 || len(sink.batches[0]) != 2 {
		t.Fatalf("batches = %+v", sink.batches)
	}
	if !bytes.Contains(sink.batches[0][0], []byte(`"request"`)) {
		t.Fatalf("first record = %s", sink.batches[0][0])
	}
	if sink.apps[0] != "shop" {
		t.Fatalf("app = %q, want shop", sink.apps[0])
	}

	// A second app's token routes to its own app name.
	if got := send(buildFrame("beef123", records)); got != "2:OK" {
		t.Fatalf("blog ack = %q", got)
	}
	deadline = time.Now().Add(2 * time.Second)
	for len(sink.batches) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(sink.apps) != 2 || sink.apps[1] != "blog" {
		t.Fatalf("apps = %v, want [shop blog]", sink.apps)
	}

	// Wrong token: still ACKed (matching official agent), but not stored.
	send(buildFrame("badbad1", records))
	time.Sleep(100 * time.Millisecond)
	if len(sink.batches) != 2 {
		t.Fatal("record with bad token was stored")
	}
}
