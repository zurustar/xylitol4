package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"xylitol4/sip"
	"xylitol4/sip/userdb"
)

func main() {
	listenAddr := flag.String("listen", ":5060", "UDP address to listen on for downstream clients (host:port)")
	upstreamAddr := flag.String("upstream", "", "Upstream SIP server UDP address (host:port)")
	upstreamBind := flag.String("upstream-bind", "", "Local UDP address to use for upstream traffic (defaults to system-chosen port)")
	routeTTL := flag.Duration("route-ttl", 5*time.Minute, "How long to remember downstream transaction routes")
	userDBPath := flag.String("user-db", "", "Path to SQLite database containing SIP user directory")
	flag.Parse()

	if *upstreamAddr == "" {
		flag.Usage()
		log.Fatal("the --upstream flag is required")
	}
	if *userDBPath == "" {
		flag.Usage()
		log.Fatal("the --user-db flag is required")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger := log.New(os.Stdout, "sip-proxy: ", log.LstdFlags|log.Lmicroseconds)

	userStore, err := userdb.OpenSQLite(*userDBPath)
	if err != nil {
		logger.Fatalf("failed to open user database %s: %v", *userDBPath, err)
	}
	defer userStore.Close()

	loadCtx, cancelLoad := context.WithTimeout(ctx, 5*time.Second)
	users, err := userStore.AllUsers(loadCtx)
	cancelLoad()
	if err != nil {
		logger.Fatalf("failed to load users from %s: %v", *userDBPath, err)
	}
	logger.Printf("loaded %d user directory entries from %s", len(users), *userDBPath)

	downstreamConn, err := net.ListenPacket("udp", *listenAddr)
	if err != nil {
		logger.Fatalf("failed to listen on %s: %v", *listenAddr, err)
	}
	defer downstreamConn.Close()

	upstreamConn, err := net.ListenPacket("udp", *upstreamBind)
	if err != nil {
		logger.Fatalf("failed to open upstream socket on %s: %v", *upstreamBind, err)
	}
	defer upstreamConn.Close()

	upstreamUDPAddr, err := net.ResolveUDPAddr("udp", *upstreamAddr)
	if err != nil {
		logger.Fatalf("failed to resolve upstream address %s: %v", *upstreamAddr, err)
	}

	proxy := sip.NewProxy()
	routes := newTransactionRouter(*routeTTL)

	logger.Printf("listening on %s, upstream %s (local upstream %s)", downstreamConn.LocalAddr(), upstreamUDPAddr, upstreamConn.LocalAddr())

	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		runDownstreamReader(ctx, proxy, downstreamConn, routes, logger)
	}()
	go func() {
		defer wg.Done()
		runUpstreamReader(ctx, proxy, upstreamConn, logger)
	}()
	go func() {
		defer wg.Done()
		runUpstreamSender(ctx, proxy, upstreamConn, upstreamUDPAddr, logger)
	}()
	go func() {
		defer wg.Done()
		runDownstreamSender(ctx, proxy, downstreamConn, routes, logger)
	}()
	go func() {
		defer wg.Done()
		routes.RunCleanup(ctx, time.Minute)
	}()

	<-ctx.Done()
	logger.Println("shutdown requested, stopping proxy")
	proxy.Stop()
	downstreamConn.Close()
	upstreamConn.Close()
	wg.Wait()
	logger.Println("shutdown complete")
}

func runDownstreamReader(ctx context.Context, proxy *sip.Proxy, conn net.PacketConn, routes *transactionRouter, logger *log.Logger) {
	buf := make([]byte, 65535)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				continue
			}
			logger.Printf("error reading from downstream: %v", err)
			continue
		}
		raw := string(buf[:n])
		msg, err := sip.ParseMessage(raw)
		if err != nil {
			logger.Printf("discarding invalid downstream datagram from %s: %v", addr.String(), err)
			continue
		}
		if msg.IsRequest() {
			if key := transactionKeyFromRequest(msg); key != "" {
				routes.Remember(key, addr)
			}
		}
		proxy.SendFromClient(msg)
	}
}

func runUpstreamReader(ctx context.Context, proxy *sip.Proxy, conn net.PacketConn, logger *log.Logger) {
	buf := make([]byte, 65535)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				continue
			}
			logger.Printf("error reading from upstream: %v", err)
			continue
		}
		raw := string(buf[:n])
		msg, err := sip.ParseMessage(raw)
		if err != nil {
			logger.Printf("discarding invalid upstream datagram from %s: %v", addr.String(), err)
			continue
		}
		proxy.SendFromServer(msg)
	}
}

func runUpstreamSender(ctx context.Context, proxy *sip.Proxy, conn net.PacketConn, dest net.Addr, logger *log.Logger) {
	for {
		msg, ok := proxy.NextToServer(250 * time.Millisecond)
		if !ok {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		payload := []byte(msg.String())
		if _, err := conn.WriteTo(payload, dest); err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			logger.Printf("failed to send upstream message: %v", err)
		}
	}
}

func runDownstreamSender(ctx context.Context, proxy *sip.Proxy, conn net.PacketConn, routes *transactionRouter, logger *log.Logger) {
	for {
		msg, ok := proxy.NextToClient(250 * time.Millisecond)
		if !ok {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		key := transactionKeyFromMessage(msg)
		if key == "" {
			logger.Printf("dropping downstream message without transaction key: %s", summarizeMessage(msg))
			continue
		}
		addr, ok := routes.Lookup(key)
		if !ok || addr == nil {
			logger.Printf("no downstream route for transaction %s; dropping message", key)
			continue
		}
		payload := []byte(msg.String())
		if _, err := conn.WriteTo(payload, addr); err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			logger.Printf("failed to send message to downstream %s: %v", addr.String(), err)
		}
	}
}

func summarizeMessage(msg *sip.Message) string {
	if msg == nil {
		return "<nil>"
	}
	if msg.IsRequest() {
		return msg.Method + " " + msg.RequestURI
	}
	return strconv.Itoa(msg.StatusCode) + " " + msg.ReasonPhrase
}

func transactionKeyFromMessage(msg *sip.Message) string {
	if msg == nil {
		return ""
	}
	if msg.IsRequest() {
		return transactionKeyFromRequest(msg)
	}
	return transactionKeyFromResponse(msg)
}

func transactionKeyFromRequest(msg *sip.Message) string {
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

func transactionKeyFromResponse(msg *sip.Message) string {
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

func topViaBranch(msg *sip.Message) string {
	if msg == nil {
		return ""
	}
	values := msg.HeaderValues("Via")
	if len(values) == 0 {
		return ""
	}
	return viaBranch(values[0])
}

func viaBranch(value string) string {
	segments := strings.Split(value, ";")
	for _, segment := range segments[1:] {
		kv := strings.SplitN(strings.TrimSpace(segment), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(kv[0]), "branch") {
			return strings.Trim(strings.TrimSpace(kv[1]), "\"")
		}
	}
	return ""
}

func cseqMethod(msg *sip.Message) string {
	if msg == nil {
		return ""
	}
	cseq := strings.TrimSpace(msg.GetHeader("CSeq"))
	if cseq == "" {
		return ""
	}
	parts := strings.Fields(cseq)
	if len(parts) < 2 {
		return ""
	}
	return strings.ToUpper(parts[1])
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
	if key == "" || addr == nil {
		return
	}
	addr = copyAddr(addr)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[key] = routeEntry{addr: addr, expires: time.Now().Add(r.ttl)}
}

func (r *transactionRouter) Lookup(key string) (net.Addr, bool) {
	if key == "" {
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
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, entry := range r.routes {
		if now.After(entry.expires) {
			delete(r.routes, key)
		}
	}
}

func (r *transactionRouter) RunCleanup(ctx context.Context, interval time.Duration) {
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
