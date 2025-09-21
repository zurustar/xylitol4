package sip

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"xylitol4/sip/userdb"
)

// SIPStackConfig describes the runtime configuration for a SIP stack instance.
type SIPStackConfig struct {
	ListenAddr      string
	UpstreamAddr    string
	UpstreamBind    string
	RouteTTL        time.Duration
	UserDBPath      string
	Logger          *log.Logger
	UserLoadTimeout time.Duration
}

// SIPStack wires together the registrar, proxy, transport, and transaction
// routing helpers used by the command-line entrypoint.
type SIPStack struct {
	cfg    SIPStackConfig
	logger *log.Logger

	mu      sync.Mutex
	started bool
	stopped bool

	userStore *userdb.SQLiteStore
	registrar *Registrar
	proxy     *Proxy

	downstreamConn net.PacketConn
	upstreamConn   net.PacketConn
	upstreamAddr   net.Addr

	routes *transactionRouter

	runCtx context.Context
	cancel context.CancelFunc

	wg sync.WaitGroup
}

// NewSIPStack validates the provided configuration and prepares a stack.
func NewSIPStack(cfg SIPStackConfig) (*SIPStack, error) {
	cfg.ListenAddr = strings.TrimSpace(cfg.ListenAddr)
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":5060"
	}

	cfg.UpstreamAddr = strings.TrimSpace(cfg.UpstreamAddr)
	if cfg.UpstreamAddr == "" {
		return nil, fmt.Errorf("sip: upstream address is required")
	}

	cfg.UpstreamBind = strings.TrimSpace(cfg.UpstreamBind)

	cfg.UserDBPath = strings.TrimSpace(cfg.UserDBPath)
	if cfg.UserDBPath == "" {
		return nil, fmt.Errorf("sip: user database path is required")
	}

	if cfg.RouteTTL <= 0 {
		cfg.RouteTTL = 5 * time.Minute
	}
	if cfg.UserLoadTimeout <= 0 {
		cfg.UserLoadTimeout = 5 * time.Second
	}

	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	return &SIPStack{
		cfg:    cfg,
		logger: logger,
	}, nil
}

// Start initialises all stack components and starts the background goroutines
// required to service SIP traffic.
func (s *SIPStack) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("sip: stack already started")
	}
	s.mu.Unlock()

	store, err := userdb.OpenSQLite(s.cfg.UserDBPath)
	if err != nil {
		return fmt.Errorf("sip: open user database %s: %w", s.cfg.UserDBPath, err)
	}
	s.userStore = store

	loadCtx, cancelLoad := context.WithTimeout(ctx, s.cfg.UserLoadTimeout)
	users, err := store.AllUsers(loadCtx)
	cancelLoad()
	if err != nil {
		s.cleanupOnError()
		return fmt.Errorf("sip: load users from %s: %w", s.cfg.UserDBPath, err)
	}
	s.logger.Printf("loaded %d user directory entries from %s", len(users), s.cfg.UserDBPath)

	downstreamConn, err := net.ListenPacket("udp", s.cfg.ListenAddr)
	if err != nil {
		s.cleanupOnError()
		return fmt.Errorf("sip: listen on %s: %w", s.cfg.ListenAddr, err)
	}
	s.downstreamConn = downstreamConn

	upstreamConn, err := net.ListenPacket("udp", s.cfg.UpstreamBind)
	if err != nil {
		s.cleanupOnError()
		return fmt.Errorf("sip: open upstream socket on %s: %w", s.cfg.UpstreamBind, err)
	}
	s.upstreamConn = upstreamConn

	upstreamAddr, err := net.ResolveUDPAddr("udp", s.cfg.UpstreamAddr)
	if err != nil {
		s.cleanupOnError()
		return fmt.Errorf("sip: resolve upstream address %s: %w", s.cfg.UpstreamAddr, err)
	}
	s.upstreamAddr = upstreamAddr

	registrar := NewRegistrar(store)
	s.registrar = registrar
	s.proxy = NewProxy(WithRegistrar(registrar))
	s.routes = newTransactionRouter(s.cfg.RouteTTL)

	s.runCtx, s.cancel = context.WithCancel(context.Background())

	s.wg.Add(5)
	go s.runDownstreamReader()
	go s.runUpstreamReader()
	go s.runUpstreamSender()
	go s.runDownstreamSender()
	go s.runRouteCleanup()

	s.logger.Printf("listening on %s, upstream %s (local upstream %s)", s.downstreamConn.LocalAddr(), s.upstreamAddr, s.upstreamConn.LocalAddr())

	s.mu.Lock()
	s.started = true
	s.stopped = false
	s.mu.Unlock()
	return nil
}

// Stop stops all background goroutines and releases resources. It is safe to
// call multiple times.
func (s *SIPStack) Stop() {
	s.mu.Lock()
	if !s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	cancel := s.cancel
	proxy := s.proxy
	downstream := s.downstreamConn
	upstream := s.upstreamConn
	store := s.userStore
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if proxy != nil {
		proxy.Stop()
	}
	if downstream != nil {
		downstream.Close()
	}
	if upstream != nil {
		upstream.Close()
	}

	s.wg.Wait()

	if store != nil {
		if err := store.Close(); err != nil {
			s.logger.Printf("error closing user database: %v", err)
		}
	}

	s.mu.Lock()
	s.started = false
	s.cancel = nil
	s.proxy = nil
	s.downstreamConn = nil
	s.upstreamConn = nil
	s.upstreamAddr = nil
	s.routes = nil
	s.registrar = nil
	s.runCtx = nil
	s.userStore = nil
	s.mu.Unlock()
}

func (s *SIPStack) cleanupOnError() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.proxy != nil {
		s.proxy.Stop()
	}
	if s.downstreamConn != nil {
		s.downstreamConn.Close()
	}
	if s.upstreamConn != nil {
		s.upstreamConn.Close()
	}
	if s.userStore != nil {
		s.userStore.Close()
	}
	s.cancel = nil
	s.proxy = nil
	s.downstreamConn = nil
	s.upstreamConn = nil
	s.upstreamAddr = nil
	s.routes = nil
	s.registrar = nil
	s.runCtx = nil
	s.userStore = nil
}

func (s *SIPStack) runDownstreamReader() {
	defer s.wg.Done()

	if s.downstreamConn == nil || s.proxy == nil || s.routes == nil {
		return
	}

	buf := make([]byte, 65535)
	for {
		n, addr, err := s.downstreamConn.ReadFrom(buf)
		if err != nil {
			if s.runCtx != nil && s.runCtx.Err() != nil {
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				continue
			}
			s.logger.Printf("error reading from downstream: %v", err)
			continue
		}
		raw := string(buf[:n])
		msg, err := ParseMessage(raw)
		if err != nil {
			s.logger.Printf("discarding invalid downstream datagram from %s: %v", addr.String(), err)
			continue
		}
		if msg.IsRequest() {
			if key := transactionKeyFromRequest(msg); key != "" {
				s.routes.Remember(key, addr)
			}
		}
		s.proxy.SendFromClient(msg)
	}
}

func (s *SIPStack) runUpstreamReader() {
	defer s.wg.Done()

	if s.upstreamConn == nil || s.proxy == nil {
		return
	}

	buf := make([]byte, 65535)
	for {
		n, addr, err := s.upstreamConn.ReadFrom(buf)
		if err != nil {
			if s.runCtx != nil && s.runCtx.Err() != nil {
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				continue
			}
			s.logger.Printf("error reading from upstream: %v", err)
			continue
		}
		raw := string(buf[:n])
		msg, err := ParseMessage(raw)
		if err != nil {
			s.logger.Printf("discarding invalid upstream datagram from %s: %v", addr.String(), err)
			continue
		}
		s.proxy.SendFromServer(msg)
	}
}

func (s *SIPStack) runUpstreamSender() {
	defer s.wg.Done()

	if s.proxy == nil || s.upstreamConn == nil || s.upstreamAddr == nil {
		return
	}

	for {
		msg, ok := s.proxy.NextToServer(250 * time.Millisecond)
		if !ok {
			if s.runCtx != nil && s.runCtx.Err() != nil {
				return
			}
			continue
		}
		payload := []byte(msg.String())
		if _, err := s.upstreamConn.WriteTo(payload, s.upstreamAddr); err != nil {
			if (s.runCtx != nil && s.runCtx.Err() != nil) || errors.Is(err, net.ErrClosed) {
				return
			}
			s.logger.Printf("failed to send upstream message: %v", err)
		}
	}
}

func (s *SIPStack) runDownstreamSender() {
	defer s.wg.Done()

	if s.proxy == nil || s.downstreamConn == nil || s.routes == nil {
		return
	}

	for {
		msg, ok := s.proxy.NextToClient(250 * time.Millisecond)
		if !ok {
			if s.runCtx != nil && s.runCtx.Err() != nil {
				return
			}
			continue
		}
		key := transactionKeyFromMessage(msg)
		if key == "" {
			s.logger.Printf("dropping downstream message without transaction key: %s", summarizeMessage(msg))
			continue
		}
		addr, ok := s.routes.Lookup(key)
		if !ok || addr == nil {
			s.logger.Printf("no downstream route for transaction %s; dropping message", key)
			continue
		}
		payload := []byte(msg.String())
		if _, err := s.downstreamConn.WriteTo(payload, addr); err != nil {
			if (s.runCtx != nil && s.runCtx.Err() != nil) || errors.Is(err, net.ErrClosed) {
				return
			}
			s.logger.Printf("failed to send message to downstream %s: %v", addr.String(), err)
		}
	}
}

func (s *SIPStack) runRouteCleanup() {
	defer s.wg.Done()

	if s.routes == nil || s.runCtx == nil {
		return
	}
	s.routes.RunCleanup(s.runCtx, time.Minute)
}

func summarizeMessage(msg *Message) string {
	if msg == nil {
		return "<nil>"
	}
	if msg.IsRequest() {
		return msg.Method + " " + msg.RequestURI
	}
	return strconv.Itoa(msg.StatusCode) + " " + msg.ReasonPhrase
}

func transactionKeyFromMessage(msg *Message) string {
	if msg == nil {
		return ""
	}
	if msg.IsRequest() {
		return transactionKeyFromRequest(msg)
	}
	return transactionKeyFromResponse(msg)
}

func transactionKeyFromRequest(msg *Message) string {
	if msg == nil {
		return ""
	}
	branch := topViaBranch(msg)
	if branch == "" {
		return ""
	}
	method := strings.ToUpper(msg.Method)
	if method == "" {
		return ""
	}
	return method + "|" + branch
}

func transactionKeyFromResponse(msg *Message) string {
	if msg == nil {
		return ""
	}
	branch := topViaBranch(msg)
	if branch == "" {
		return ""
	}
	method := cseqMethod(msg)
	if method == "" {
		return ""
	}
	return method + "|" + branch
}

func copyAddr(addr net.Addr) net.Addr {
	if addr == nil {
		return nil
	}
	if udp, ok := addr.(*net.UDPAddr); ok {
		clone := *udp
		return &clone
	}
	return addr
}

type transactionRouter struct {
	mu     sync.RWMutex
	routes map[string]routeEntry
	ttl    time.Duration
}

type routeEntry struct {
	addr    net.Addr
	expires time.Time
}

func newTransactionRouter(ttl time.Duration) *transactionRouter {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &transactionRouter{
		routes: make(map[string]routeEntry),
		ttl:    ttl,
	}
}

func (r *transactionRouter) Remember(key string, addr net.Addr) {
	if r == nil || key == "" || addr == nil {
		return
	}
	addr = copyAddr(addr)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[key] = routeEntry{addr: addr, expires: time.Now().Add(r.ttl)}
}

func (r *transactionRouter) Lookup(key string) (net.Addr, bool) {
	if r == nil || key == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.routes[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expires) {
		delete(r.routes, key)
		return nil, false
	}
	entry.expires = time.Now().Add(r.ttl)
	r.routes[key] = entry
	return entry.addr, true
}

func (r *transactionRouter) cleanup(now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, entry := range r.routes {
		if now.After(entry.expires) {
			delete(r.routes, key)
		}
	}
}

func (r *transactionRouter) RunCleanup(ctx context.Context, interval time.Duration) {
	if r == nil {
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			r.cleanup(now)
		}
	}
}
