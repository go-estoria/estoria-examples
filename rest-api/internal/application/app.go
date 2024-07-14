package application

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-estoria/estoria-examples/basic-rest-api/internal/database"
	"github.com/gofrs/uuid/v5"
)

// AccountStorage provides APIs for interacting with the database layer.
// The database layer is responsible for creating, reading, updating, and deleting Accounts.
type AccountStorage interface {
	CreateAccount(ctx context.Context, initialUser string) (*database.Account, error)
	GetAccount(ctx context.Context, accountID uuid.UUID) (*database.Account, error)
	DeleteAccount(ctx context.Context, accountID uuid.UUID, reason string) error
}

// App holds the dependencies and defines the request handlers for the application.
type App struct {
	http *http.Server
	db   AccountStorage
}

// New creates a new App with the provided dependencies.
func New(server *http.Server, db AccountStorage) *App {
	app := &App{
		http: server,
		db:   db,
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

// Run starts the application and listens for incoming requests.
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
