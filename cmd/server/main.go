package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"marketplace/internal/app"
)

func run(log *slog.Logger) error {
	c, err := app.LoadConfig()
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", c.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		return err
	}
	s, err := app.New(db, c, log)
	if err != nil {
		return err
	}
	if err = s.BootstrapAdmin(ctx); err != nil {
		return err
	}
	srv := &http.Server{
		Addr: c.Addr, Handler: s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second, WriteTimeout: 20 * time.Second,
		IdleTimeout: 60 * time.Second,
	}
	stop, done := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer done()
	ch := make(chan error, 1)
	go func() { ch <- srv.ListenAndServe() }()
	log.Info("server started", "addr", c.Addr)
	select {
	case err = <-ch:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-stop.Done():
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("server stopped", "error", err.Error())
		os.Exit(1)
	}
}
