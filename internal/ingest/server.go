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
	"strconv"
	"time"
)

// Sink receives fully-parsed record batches tagged with the sending app.
type Sink interface {
	InsertRecords(ctx context.Context, records []json.RawMessage, app string) (int, error)
}

// AppResolver maps a token hash to a registered app on every frame, so
// apps created in the panel start ingesting without a restart.
// found=false with anyApps=false means no apps are registered (accept
// anything); found=false with anyApps=true means the token is invalid.
type AppResolver interface {
	ResolveApp(ctx context.Context, tokenHash string) (name string, found, anyApps bool, err error)
}

// Server implements the Laravel Nightwatch agent wire protocol:
//
//	{length}:v1:{tokenHash}:{payload}
//
// where length = len("v1") + 1 + len(tokenHash) + 1 + len(payload).
// The payload is either the literal "PING" or a JSON array of records.
// Every complete frame is acknowledged with "2:OK" and the connection closed.
type Server struct {
	addr        string
	resolver    AppResolver
	readTimeout time.Duration
	sink        Sink
	log         *slog.Logger
	ln          net.Listener
}

const (
	payloadVersion = "v1"
	maxFrameSize   = 64 << 20 // 64 MiB safety cap
	ack            = "2:OK"
)

func New(addr string, resolver AppResolver, readTimeout time.Duration, sink Sink, log *slog.Logger) *Server {
	return &Server{addr: addr, resolver: resolver, readTimeout: readTimeout, sink: sink, log: log}
}

func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("ingest listen %s: %w", s.addr, err)
	}
	s.ln = ln
	s.log.Info("ingest listening", "addr", s.addr)
	return nil
}

func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.ln.Close()
	}()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(s.readTimeout))

	payload, tokenHash, err := readFrame(conn)
	if err != nil {
		s.log.Warn("bad frame", "remote", conn.RemoteAddr().String(), "error", err)
		return
	}

	resolveCtx, cancelResolve := context.WithTimeout(ctx, 5*time.Second)
	app, found, anyApps, err := s.resolver.ResolveApp(resolveCtx, tokenHash)
	cancelResolve()
	if err != nil {
		s.log.Error("app resolution failed", "error", err)
		conn.Write([]byte(ack))
		return
	}
	if anyApps && !found {
		s.log.Warn("invalid token hash", "remote", conn.RemoteAddr().String(), "got", tokenHash)
		// The official agent still ACKs before validating, so we do the same:
		// the app treats a missing ACK as an agent failure and logs noise.
		conn.Write([]byte(ack))
		return
	}

	// ACK as soon as the frame is complete, mirroring the official agent.
	if _, err := conn.Write([]byte(ack)); err != nil {
		s.log.Warn("ack write failed", "error", err)
	}

	if bytes.Equal(payload, []byte("PING")) {
		s.log.Debug("ping received", "remote", conn.RemoteAddr().String())
		return
	}

	var records []json.RawMessage
	if err := json.Unmarshal(payload, &records); err != nil {
		s.log.Warn("invalid JSON payload", "error", err, "size", len(payload))
		return
	}
	if len(records) == 0 {
		return
	}

	insertCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	n, err := s.sink.InsertRecords(insertCtx, records, app)
	if err != nil {
		s.log.Error("insert failed", "error", err, "records", len(records))
		return
	}
	s.log.Info("ingested", "records", n, "app", app, "remote", conn.RemoteAddr().String())
}

// readFrame parses one protocol frame from r.
func readFrame(r io.Reader) (payload []byte, tokenHash string, err error) {
	br := newByteReader(r)

	// Length prefix: ASCII digits terminated by ':'.
	var lengthBuf []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, "", fmt.Errorf("reading length: %w", err)
		}
		if b == ':' {
			break
		}
		if b < '0' || b > '9' || len(lengthBuf) > 12 {
			return nil, "", errors.New("malformed length prefix")
		}
		lengthBuf = append(lengthBuf, b)
	}
	length, err := strconv.Atoi(string(lengthBuf))
	if err != nil || length <= 0 || length > maxFrameSize {
		return nil, "", fmt.Errorf("invalid frame length %q", lengthBuf)
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, "", fmt.Errorf("reading body (%d bytes): %w", length, err)
	}

	// body = version ':' tokenHash ':' payload
	parts := bytes.SplitN(body, []byte{':'}, 3)
	if len(parts) != 3 {
		return nil, "", errors.New("malformed frame body")
	}
	if string(parts[0]) != payloadVersion {
		return nil, "", fmt.Errorf("unsupported payload version %q", parts[0])
	}
	return parts[2], string(parts[1]), nil
}

// byteReader is a tiny buffered reader that still allows io.ReadFull on the rest.
type byteReader struct {
	r   io.Reader
	buf [1]byte
}

func newByteReader(r io.Reader) *byteReader { return &byteReader{r: r} }

func (b *byteReader) ReadByte() (byte, error) {
	_, err := io.ReadFull(b.r, b.buf[:])
	return b.buf[0], err
}

func (b *byteReader) Read(p []byte) (int, error) { return b.r.Read(p) }
