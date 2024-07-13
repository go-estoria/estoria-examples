package application

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/gofrs/uuid/v5"
)

// AccountDTO is a data transfer object for an account.
type AccountDTO struct {
	ID        uuid.UUID  `json:"id"`
	Users     []string   `json:"users"`
	Balance   int        `json:"balance"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

func (a *App) HandleCreateAccount(w http.ResponseWriter, r *http.Request) {
	slog.Info("creating account")

	defer r.Body.Close()
	req := make(map[string]string)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	slog.Info("request body", "req", req)

	account, err := a.stg.CreateAccount(r.Context(), req["user"])
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("account created", "id", account.ID)

	resp := AccountDTO{
		ID:        account.ID,
		Users:     account.Users,
		Balance:   account.Balance,
		CreatedAt: account.CreatedAt,
		DeletedAt: account.DeletedAt,
	}

	b, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(b)

	slog.Info("account created", "account", account.String())
}

func (a *App) HandleGetAccount(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("id")
	if accountID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	id, err := uuid.FromString(accountID)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	account, err := a.stg.GetAccount(r.Context(), id)
	if err != nil {
		http.Error(w, "error getting account", http.StatusInternalServerError)
		return
	}

	resp := AccountDTO{
		ID:        account.ID,
		Users:     account.Users,
		Balance:   account.Balance,
		CreatedAt: account.CreatedAt,
		DeletedAt: account.DeletedAt,
	}

	b, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	slog.Info("account retrieved", "account", account.String())

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

func (a *App) HandleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("id")
	if accountID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	id, err := uuid.FromString(accountID)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	defer r.Body.Close()
	req := make(map[string]string)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	reason, ok := req["reason"]
	if !ok || reason == "" {
		http.Error(w, "missing reason", http.StatusBadRequest)
		return
	}

	if err := a.stg.DeleteAccount(r.Context(), id, reason); err != nil {
		http.Error(w, "error deleting account", http.StatusInternalServerError)
		return
	}

	slog.Info("account deleted", "id", id, "reason", reason)
	w.WriteHeader(http.StatusNoContent)
}
