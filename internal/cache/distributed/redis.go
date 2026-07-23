package distributed

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// Dialer dials a network address, returning a connection. It mirrors
// net.Dialer.DialContext so tests can inject a fake connection without
// touching the network.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// defaultDialTimeout caps how long a dial may block.
const defaultDialTimeout = 5 * time.Second

// defaultCmdTimeout caps how long a single command may take when the
// caller's context carries no deadline.
const defaultCmdTimeout = 3 * time.Second

// RedisBackend speaks the Redis Serialization Protocol (RESP) over a TCP
// connection using only the standard library, so the cache can front a
// real Redis (or any Redis-compatible server such as Valkey, Dragonfly,
// or KeyDB) without a third-party client dependency.
//
// It holds a single connection guarded by a mutex and reconnects lazily
// on error. That is sufficient for the milestone tier; a production
// deployment would pool connections behind the same Backend interface.
type RedisBackend struct {
	addr     string
	password string
	db       int
	timeout  time.Duration
	dialer   Dialer

	mu   sync.Mutex
	conn net.Conn
	br   *bufio.Reader
}

// RedisOption configures a RedisBackend during construction.
type RedisOption func(*RedisBackend)

// WithPassword sets the AUTH password sent after connecting.
func WithPassword(p string) RedisOption {
	return func(r *RedisBackend) { r.password = p }
}

// WithDB selects the logical database index via SELECT after connecting.
func WithDB(db int) RedisOption {
	return func(r *RedisBackend) { r.db = db }
}

// WithRedisTimeout sets the per-command timeout used when the caller's
// context carries no deadline.
func WithRedisTimeout(d time.Duration) RedisOption {
	return func(r *RedisBackend) {
		if d > 0 {
			r.timeout = d
		}
	}
}

// WithDialer injects the dialer used to reach Redis. Mainly for tests.
func WithDialer(d Dialer) RedisOption {
	return func(r *RedisBackend) { r.dialer = d }
}

// NewRedisBackend returns a RedisBackend targeting addr (host:port).
// The connection is established lazily on the first command.
func NewRedisBackend(addr string, opts ...RedisOption) *RedisBackend {
	r := &RedisBackend{
		addr:    addr,
		timeout: defaultCmdTimeout,
		dialer:  stdDialer,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// stdDialer adapts net.Dialer to the Dialer function signature.
var stdDialer Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: defaultDialTimeout}
	return d.DialContext(ctx, network, addr)
}

// Name reports the backend name for telemetry.
func (r *RedisBackend) Name() string { return "redis" }

// Ping verifies connectivity by sending PING.
func (r *RedisBackend) Ping(ctx context.Context) error {
	_, err := r.exec(ctx, "PING")
	return err
}

// Get returns the bytes stored under key, or ErrNotFound for a nil bulk.
func (r *RedisBackend) Get(ctx context.Context, key string) ([]byte, error) {
	v, err := r.exec(ctx, "GET", key)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, ErrNotFound
	}
	b, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("distributed cache: unexpected GET reply type %T", v)
	}
	return b, nil
}

// Set stores value under key. When ttl > 0 it is applied via the PX
// (milliseconds) option so the server evicts the key on expiry.
func (r *RedisBackend) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	args := []string{"SET", key, string(value)}
	if ttl > 0 {
		args = append(args, "PX", strconv.FormatInt(ttl.Milliseconds(), 10))
	}
	_, err := r.exec(ctx, args...)
	return err
}

// Del removes the given keys and returns the count the server deleted.
func (r *RedisBackend) Del(ctx context.Context, keys ...string) (int, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	args := append([]string{"DEL"}, keys...)
	v, err := r.exec(ctx, args...)
	if err != nil {
		return 0, err
	}
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("distributed cache: unexpected DEL reply type %T", v)
	}
	return int(n), nil
}

// Close releases the server connection, if any.
func (r *RedisBackend) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropLocked()
}

// exec writes one command and reads its reply, reconnecting after a
// connection error. The mutex serializes commands over the single conn.
func (r *RedisBackend) exec(ctx context.Context, args ...string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.ensureConnectedLocked(ctx); err != nil {
		return nil, err
	}

	// Apply a deadline: prefer the caller's context, else the backend
	// timeout, so a hung server cannot stall callers indefinitely.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(r.timeout)
	}
	_ = r.conn.SetDeadline(deadline)

	if _, err := r.conn.Write(encodeCommand(args)); err != nil {
		_ = r.dropLocked()
		return nil, fmt.Errorf("distributed cache: redis write: %w", err)
	}
	v, err := readReply(r.br)
	if err != nil {
		_ = r.dropLocked()
		return nil, err
	}
	return v, nil
}

// ensureConnectedLocked dials and authenticates on first use. Caller
// must hold r.mu.
func (r *RedisBackend) ensureConnectedLocked(ctx context.Context) error {
	if r.conn != nil {
		return nil
	}

	conn, err := r.dialer(ctx, "tcp", r.addr)
	if err != nil {
		return fmt.Errorf("distributed cache: dial %s: %w", r.addr, err)
	}
	r.conn = conn
	r.br = bufio.NewReaderSize(conn, 64*1024)

	if r.password != "" {
		if _, err := r.conn.Write(encodeCommand([]string{"AUTH", r.password})); err != nil {
			_ = r.dropLocked()
			return fmt.Errorf("distributed cache: redis auth write: %w", err)
		}
		if _, err := readReply(r.br); err != nil {
			_ = r.dropLocked()
			return fmt.Errorf("distributed cache: redis auth: %w", err)
		}
	}
	if r.db != 0 {
		if _, err := r.conn.Write(encodeCommand([]string{"SELECT", strconv.Itoa(r.db)})); err != nil {
			_ = r.dropLocked()
			return fmt.Errorf("distributed cache: redis select write: %w", err)
		}
		if _, err := readReply(r.br); err != nil {
			_ = r.dropLocked()
			return fmt.Errorf("distributed cache: redis select: %w", err)
		}
	}
	return nil
}

// dropLocked closes and forgets the current connection. Caller must hold
// r.mu.
func (r *RedisBackend) dropLocked() error {
	var err error
	if r.conn != nil {
		err = r.conn.Close()
		r.conn = nil
		r.br = nil
	}
	return err
}

// encodeCommand renders a Redis command as a RESP array of bulk strings.
// It is exported-by-package for unit testing of the wire codec.
func encodeCommand(args []string) []byte {
	var buf bytes.Buffer
	buf.WriteString("*")
	buf.WriteString(strconv.Itoa(len(args)))
	buf.WriteString("\r\n")
	for _, a := range args {
		b := []byte(a)
		buf.WriteString("$")
		buf.WriteString(strconv.Itoa(len(b)))
		buf.WriteString("\r\n")
		buf.Write(b)
		buf.WriteString("\r\n")
	}
	return buf.Bytes()
}

// readReply parses a single RESP reply from br. It returns:
//   - string, for a simple-string reply ("+OK"),
//   - int64, for an integer reply,
//   - []byte, for a bulk-string reply,
//   - nil, for a nil bulk ("$-1") or nil array ("*-1"),
//   - []any, for an array reply,
//
// and an error for a RESP error reply ("-ERR ...") or a protocol fault.
func readReply(br *bufio.Reader) (any, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("distributed cache: read reply: %w", err)
	}
	if len(line) < 3 || line[len(line)-2] != '\r' || line[len(line)-1] != '\n' {
		return nil, fmt.Errorf("distributed cache: malformed reply %q", line)
	}
	payload := line[:len(line)-2]
	if len(payload) == 0 {
		return nil, errors.New("distributed cache: empty reply")
	}

	switch payload[0] {
	case '+': // simple string
		return payload[1:], nil
	case '-': // error
		return nil, errors.New(payload[1:])
	case ':': // integer
		n, err := strconv.ParseInt(payload[1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("distributed cache: bad integer %q: %w", payload, err)
		}
		return n, nil
	case '$': // bulk string
		n, err := strconv.ParseInt(payload[1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("distributed cache: bad bulk length %q: %w", payload, err)
		}
		if n < 0 {
			return nil, nil // nil bulk
		}
		buf := make([]byte, n+2) // include trailing \r\n
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, fmt.Errorf("distributed cache: read bulk: %w", err)
		}
		return buf[:n], nil
	case '*': // array
		n, err := strconv.ParseInt(payload[1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("distributed cache: bad array length %q: %w", payload, err)
		}
		if n < 0 {
			return nil, nil // nil array
		}
		arr := make([]any, 0, n)
		for i := int64(0); i < n; i++ {
			elem, err := readReply(br)
			if err != nil {
				return nil, err
			}
			arr = append(arr, elem)
		}
		return arr, nil
	default:
		return nil, fmt.Errorf("distributed cache: unknown reply tag %q", payload[0])
	}
}
