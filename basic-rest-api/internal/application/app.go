package application

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-estoria/estoria-examples/basic-rest-api/internal/storage"
	"github.com/gofrs/uuid/v5"
)

type AccountStorage interface {
	CreateAccount(ctx context.Context, initialUser string) (*storage.Account, error)
	GetAccount(ctx context.Context, accountID uuid.UUID) (*storage.Account, error)
	DeleteAccount(ctx context.Context, accountID uuid.UUID, reason string) error
}

type App struct {
	http *http.Server
	stg  AccountStorage
}

func New(server *http.Server, stg AccountStorage) *App {
	app := &App{
		http: server,
		stg:  stg,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/accounts", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			app.HandleCreateAccount(w, r)
		case http.MethodGet:
			app.HandleGetAccount(w, r)
		case http.MethodDelete:
			app.HandleDeleteAccount(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	server.Handler = mux

	return app
}

func (a *App) Run(ctx context.Context) {
	go func() {
		slog.Info("listening", "addr", a.http.Addr)
		if err := a.http.ListenAndServe(); err != nil {
			slog.Error("http server error", "error", err)
		}
	}()

	<-ctx.Done()

	slog.Info("shutting down")
	if err := a.http.Shutdown(ctx); err != nil {
		slog.Error("http server shutdown error", "error", err)
	}
}
