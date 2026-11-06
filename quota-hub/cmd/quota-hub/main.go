package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"quota-hub/internal/collectors"
	"quota-hub/internal/config"
	"quota-hub/internal/engine"
	"quota-hub/internal/httpapi"
	"quota-hub/internal/model"
	"quota-hub/internal/store"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("quota-hub", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "quota-hub.local.json", "secret-free JSON configuration")
	check := flags.Bool("check-config", false, "validate only; no credentials, state, network or HTTP listener")
	once := flags.Bool("once", false, "perform due collections once, respecting persisted cooldown; no HTTP listener")
	resumeAuth := flags.String("resume-auth", "", "operator-only: reschedule one auth failure after correcting its source; no collection, never clears rate limits")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || (*check && *once) || (*resumeAuth != "" && (*check || *once)) {
		fmt.Fprintln(stderr, "invalid command arguments")
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	specs, _ := cfg.Specs() // Load already validated, with no credential/file access.
	if *check {
		ids := make([]string, 0, len(specs))
		for _, spec := range specs {
			ids = append(ids, spec.ID)
		}
		_ = json.NewEncoder(stdout).Encode(struct {
			Valid   bool     `json:"valid"`
			Enabled []string `json:"enabled_subscriptions"`
		}{true, ids})
		return 0
	}
	var token string
	if cfg.HTTPAuth != "none" {
		token = os.Getenv(cfg.HTTPTokenEnv)
	}
	if cfg.HTTPAuth != "none" && !*once && *resumeAuth == "" && (len(token) < 24 || strings.IndexFunc(token, unicode.IsSpace) >= 0) {
		fmt.Fprintln(stderr, "HTTP bearer-token environment variable must contain at least 24 non-whitespace bytes")
		return 1
	}
	definitions := make([]model.Definition, 0, len(specs))
	workers := make([]engine.Worker, 0, len(specs))
	for _, spec := range specs {
		collector, err := collectors.New(spec, collectors.Options{})
		if err != nil {
			fmt.Fprintln(stderr, "invalid collector configuration")
			return 1
		}
		definitions = append(definitions, spec.Definition)
		workers = append(workers, engine.Worker{Spec: spec, Collector: collector})
	}
	s, err := store.Open(cfg.DataDir, definitions)
	if err != nil {
		fmt.Fprintln(stderr, "cannot initialize private storage or acquire single-writer lock")
		return 1
	}
	defer s.Close()
	if *resumeAuth != "" {
		sub, ok := s.Get(*resumeAuth)
		hybrid, hasHybrid := s.Hybrid(*resumeAuth)
		failure := sub.Error
		if hasHybrid && hybrid.RemoteError != nil {
			failure = hybrid.RemoteError
		}
		if !ok || failure == nil || failure.HTTPStatus == 429 || failure.Code == "rate_limited" || failure.RetryAt != nil {
			fmt.Fprintln(stderr, "resume-auth requires an existing authentication error without a server retry deadline")
			return 1
		}
		isAuth := failure.HTTPStatus == 401 || failure.HTTPStatus == 403 ||
			failure.Code == "auth_required" || failure.Code == "access_denied" || failure.Code == "credentials_unavailable"
		if !isAuth {
			fmt.Fprintln(stderr, "resume-auth cannot clear non-authentication errors or rate-limit pauses")
			return 1
		}
		now := time.Now().UTC()
		var rescheduleError error
		if hasHybrid {
			hybrid.NextRemoteAttemptAt = &now
			hybrid.RemoteError = &model.ErrorInfo{Code: "auth_source_reconfigured"}
			rescheduleError = s.SetHybrid(*resumeAuth, hybrid, now)
		} else {
			rescheduleError = s.Commit(model.Update{ID: *resumeAuth, AttemptAt: now, NextAttemptAt: now,
				Failure: &model.ErrorInfo{Code: "auth_source_reconfigured"}})
		}
		if rescheduleError != nil {
			fmt.Fprintln(stderr, "cannot persist authentication reschedule")
			return 1
		}
		_ = json.NewEncoder(stdout).Encode(struct {
			Resumed string `json:"resumed"`
		}{*resumeAuth})
		return 0
	}
	e, err := engine.New(s, workers, engine.Options{
		Jitter: 0.15, BackoffBase: time.Minute, BackoffMax: time.Hour, AuthRetry: 30 * time.Minute,
	})
	if err != nil {
		fmt.Fprintln(stderr, "cannot initialize collection scheduler")
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *once {
		exit := 0
		for _, spec := range specs {
			if _, err := e.Poll(ctx, spec.ID); err != nil {
				fmt.Fprintln(stderr, "collection or persistence failed; see credential-free state")
				exit = 1
			}
			if sub, ok := s.Get(spec.ID); ok && sub.Error != nil {
				exit = 1
			}
		}
		if err := json.NewEncoder(stdout).Encode(s.Snapshot(time.Now())); err != nil {
			return 1
		}
		return exit
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintln(stderr, "cannot bind configured LAN listener; no collection started")
		return 1
	}
	handler := httpapi.New(s, token, time.Now)
	if cfg.HTTPAuth == "none" {
		handler = httpapi.NewUnauthenticated(s, time.Now)
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	engineDone := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() { engineDone <- e.Run(ctx) }()
	go func() { serverDone <- server.Serve(listener) }()
	fmt.Fprintf(stderr, "quota-hub listening on %s; %d configured subscriptions\n", cfg.Listen, len(specs))
	exit, engineStopped := 0, false
	select {
	case <-ctx.Done():
	case err := <-engineDone:
		engineStopped = true
		if err != nil {
			fmt.Fprintln(stderr, "collection scheduler stopped")
			exit = 1
		}
	case err := <-serverDone:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(stderr, "HTTP server stopped unexpectedly")
			exit = 1
		}
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := server.Shutdown(shutdown); err != nil {
		_ = server.Close()
		exit = 1
	}
	if !engineStopped {
		select {
		case <-engineDone:
		case <-shutdown.Done():
			fmt.Fprintln(stderr, "collection shutdown timed out")
			exit = 1
		}
	}
	return exit
}
