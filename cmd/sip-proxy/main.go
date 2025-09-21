package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"xylitol4/sip"
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

	stack, err := sip.NewSIPStack(sip.SIPStackConfig{
		ListenAddr:      *listenAddr,
		UpstreamAddr:    *upstreamAddr,
		UpstreamBind:    *upstreamBind,
		RouteTTL:        *routeTTL,
		UserDBPath:      *userDBPath,
		Logger:          logger,
		UserLoadTimeout: 5 * time.Second,
	})
	if err != nil {
		logger.Fatalf("failed to construct SIP stack: %v", err)
	}

	if err := stack.Start(ctx); err != nil {
		logger.Fatalf("failed to start SIP stack: %v", err)
	}

	<-ctx.Done()
	logger.Println("shutdown requested, stopping proxy")
	stack.Stop()
	logger.Println("shutdown complete")
}
