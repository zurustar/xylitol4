package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"xylitol4/internal/userweb"
	"xylitol4/sip"
	"xylitol4/sip/userdb"
)

func main() {
	listenAddr := flag.String("listen", ":5060", "UDP address to listen on for downstream clients (host:port)")
	upstreamAddr := flag.String("upstream", "", "Upstream SIP server UDP address (host:port)")
	upstreamBind := flag.String("upstream-bind", "", "Local UDP address to use for upstream traffic (defaults to system-chosen port)")
	routeTTL := flag.Duration("route-ttl", 5*time.Minute, "How long to remember downstream transaction routes")
	userDBPath := flag.String("user-db", "", "Path to SQLite database containing SIP user directory")
	httpListen := flag.String("http-listen", ":8080", "HTTP address to listen on (host:port)")
	adminUser := flag.String("admin-user", "", "Username required for admin endpoints")
	adminPass := flag.String("admin-pass", "", "Password required for admin endpoints")
	flag.Parse()

	if strings.TrimSpace(*userDBPath) == "" {
		flag.Usage()
		log.Fatal("the --user-db flag is required")
	}

	if *upstreamAddr == "" {
		log.Println("--upstream not provided; requests will be routed using local registrations or Request-URI resolution")
	}

	trimmedAdminUser := strings.TrimSpace(*adminUser)
	trimmedAdminPass := strings.TrimSpace(*adminPass)
	httpEnabled := trimmedAdminUser != "" || trimmedAdminPass != ""
	if httpEnabled && (trimmedAdminUser == "" || trimmedAdminPass == "") {
		log.Fatal("both --admin-user and --admin-pass must be provided to enable the web interface")
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

	var (
		httpServer  *http.Server
		httpErrCh   chan error
		httpErr     error
		errReported bool
		webStore    *userdb.SQLiteStore
		webLogger   *log.Logger
	)

	if httpEnabled {
		store, err := userdb.OpenSQLite(*userDBPath)
		if err != nil {
			logger.Fatalf("failed to open user database for web interface: %v", err)
		}
		webStore = store
		webLogger = log.New(os.Stdout, "user-web: ", log.LstdFlags|log.Lmicroseconds)
		webServer, err := userweb.New(userweb.Config{
			Store:     store,
			AdminUser: trimmedAdminUser,
			AdminPass: trimmedAdminPass,
			Logger:    webLogger,
		})
		if err != nil {
			logger.Fatalf("failed to construct user web server: %v", err)
		}

		httpServer = &http.Server{
			Addr:         *httpListen,
			Handler:      webServer.Handler(),
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 10 * time.Second,
		}
		httpErrCh = make(chan error, 1)
		go func() {
			webLogger.Printf("user web interface listening on %s", *httpListen)
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				httpErrCh <- err
				return
			}
			httpErrCh <- nil
		}()
		defer func() {
			if webStore != nil {
				if err := webStore.Close(); err != nil {
					webLogger.Printf("error closing user database: %v", err)
				}
			}
		}()
	} else {
		logger.Println("user web interface disabled; provide --admin-user and --admin-pass to enable it")
	}

	if httpErrCh != nil {
		select {
		case err := <-httpErrCh:
			httpErr = err
			errReported = true
			if err != nil {
				logger.Printf("user web interface error: %v", err)
				cancel()
			}
		case <-ctx.Done():
		}
	} else {
		<-ctx.Done()
	}

	<-ctx.Done()

	logger.Println("shutdown requested, stopping proxy")
	if httpServer != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := httpServer.Shutdown(shutdownCtx); err != nil && err != http.ErrServerClosed {
			logger.Printf("error shutting down user web interface: %v", err)
		}
		shutdownCancel()
		if !errReported {
			if err := <-httpErrCh; err != nil {
				httpErr = err
			}
		}
		if httpErr != nil {
			logger.Printf("user web interface terminated with error: %v", httpErr)
		}
	}

	stack.Stop()
	logger.Println("shutdown complete")
}
