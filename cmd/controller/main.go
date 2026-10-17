// Command controller keeps one short-lived Railway service per queued GitHub Actions job.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/farzai/github-runner-railway/internal/config"
	"github.com/farzai/github-runner-railway/internal/controller"
	"github.com/farzai/github-runner-railway/internal/ghapp"
	"github.com/farzai/github-runner-railway/internal/railway"
	"github.com/farzai/github-runner-railway/internal/webhook"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		log.Error("startup", "err", err.Error())
		os.Exit(1)
	}
	gh, err := ghapp.New(ghapp.Config{AppID: cfg.AppID, PrivateKey: cfg.PrivateKey, Label: cfg.Label, RunnerGroupID: cfg.RunnerGroupID})
	if err != nil {
		log.Error("startup", "err", err.Error())
		os.Exit(1)
	}
	rw := railway.New(railway.Config{Token: cfg.RailwayToken, ProjectID: cfg.RailwayProject, EnvironmentID: cfg.RailwayEnv, Image: cfg.Image, Prefix: cfg.ServicePrefix})
	alert := func(msg string) {
		log.Error("ALERT", "alert", msg) // not "msg": slog writes the message under that key
		if cfg.AlertURL == "" {
			return
		}
		body, _ := json.Marshal(map[string]string{"text": msg})
		if res, err := (&http.Client{Timeout: 10 * time.Second}).Post(cfg.AlertURL, "application/json", bytes.NewReader(body)); err == nil {
			res.Body.Close()
		}
	}
	c := controller.New(controller.Config{
		Scopes: cfg.Scopes, Prefix: cfg.ServicePrefix, MaxRunners: cfg.MaxRunners, MaxCreatePerCycle: cfg.MaxCreatePerCycle,
		StartupGrace: cfg.StartupGrace, IdleTTL: cfg.IdleTTL, MaxAge: cfg.MaxAge, AlertAfter: cfg.AlertAfter,
		Interval: cfg.Interval, FailAlertAfter: cfg.FailAlertAfter, Alert: alert, Log: log,
	}, gh, rw)

	mux := http.NewServeMux()
	mux.Handle("/webhook", webhook.Handler(cfg.WebhookSecret, cfg.Label, func(e webhook.Event) {
		log.Info("workflow_job", "action", e.Action, "repo", e.Repo)
		c.OnEvent(e)
	}))
	mux.Handle("/healthz", c.HealthHandler())
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go c.Run(ctx)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("controller up", "config", cfg.String())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("server", "err", err.Error())
		os.Exit(1)
	}
}
