package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-estoria/estoria"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore"
	"github.com/go-estoria/estoria/projection"
	"github.com/go-estoria/estoria/projection/checkpointstore"
	"github.com/go-estoria/estoria/projection/lifecycle"
	"github.com/gofrs/uuid/v5"
)

// A server exposes the ledger's command API, the read model behind the
// router, and the rebuild console's lifecycle controls. It owns at most one
// rebuild run at a time: lifecycle commands that need the certified run —
// Promote above all — act on that handle, while commands that only need the
// durable record resume a fresh handle when no run is active.
type server struct {
	appCtx       context.Context
	accounts     *aggregatestore.EventSourcedStore[Account]
	orchestrator *lifecycle.Orchestrator
	router       lifecycle.Router
	model        *readModel
	checkpoints  checkpointstore.Store
	serving      *servingManager
	traffic      *trafficGenerator
	log          estoria.Logger

	mu  sync.Mutex
	run *rebuildRun
}

// A rebuildRun is one Run of a rebuild handle: the handle for commands, and
// the run's result once it finishes.
type rebuildRun struct {
	handle   *lifecycle.Rebuild
	cancel   context.CancelFunc
	done     chan struct{}
	err      error
	finished bool
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/accounts", s.handleListAccounts)
	mux.HandleFunc("POST /api/accounts", s.handleOpenAccount)
	mux.HandleFunc("POST /api/accounts/{id}/deposit", s.handleDeposit)
	mux.HandleFunc("POST /api/accounts/{id}/withdraw", s.handleWithdraw)
	mux.HandleFunc("POST /api/rebuild", s.handleBeginRebuild)
	mux.HandleFunc("POST /api/rebuild/resume", s.handleResumeRebuild)
	mux.HandleFunc("POST /api/rebuild/promote", s.handlePromote)
	mux.HandleFunc("POST /api/rebuild/rollback", s.handleRollback)
	mux.HandleFunc("POST /api/rebuild/abandon", s.handleAbandon)
	mux.HandleFunc("POST /api/rebuild/retire", s.handleRetire)
	mux.HandleFunc("POST /api/policy", s.handleSetPolicy)
	mux.HandleFunc("POST /api/traffic", s.handleTraffic)
	mux.Handle("/", http.FileServer(http.Dir("web")))

	return mux
}

// --- overview ---

type versionRef struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

func refOf(id projection.ID) versionRef {
	return versionRef{ID: id.String(), Version: id.Version}
}

type checkpointView struct {
	Position   int64     `json:"position"`
	UpdatedAt  time.Time `json:"updatedAt"`
	AgeSeconds float64   `json:"ageSeconds"`
}

type versionCard struct {
	Roles      []string        `json:"roles"`
	Ref        versionRef      `json:"ref"`
	Enriched   bool            `json:"enriched"`
	Exists     bool            `json:"exists"`
	Rows       []balanceRow    `json:"rows"`
	Checkpoint *checkpointView `json:"checkpoint"`
}

type attemptView struct {
	ID            string      `json:"id"`
	Phase         string      `json:"phase"`
	Reason        string      `json:"reason"`
	Target        versionRef  `json:"target"`
	Previous      *versionRef `json:"previous,omitempty"`
	Runner        string      `json:"runner,omitempty"`
	ClaimStanding bool        `json:"claimStanding"`
	InitiatedAt   time.Time   `json:"initiatedAt"`
	CaughtUpPos   int64       `json:"caughtUpPos,omitempty"`
}

type runView struct {
	Active        bool   `json:"active"`
	Finished      bool   `json:"finished"`
	Result        string `json:"result,omitempty"`
	Failed        bool   `json:"failed"`
	ClaimStanding bool   `json:"claimStanding"`
	Displaced     bool   `json:"displaced"`
}

type policyView struct {
	Generation  int64    `json:"generation"`
	Witnesses   []string `json:"witnesses"`
	Unwitnessed bool     `json:"unwitnessed"`
}

type overview struct {
	Projection      string        `json:"projection"`
	Live            *versionRef   `json:"live"`
	CutoverRevision int64         `json:"cutoverRevision"`
	Allocated       int           `json:"allocated"`
	Attempt         *attemptView  `json:"attempt"`
	Versions        []versionCard `json:"versions"`
	Run             runView       `json:"run"`
	Policy          policyView    `json:"policy"`
	Serving         *versionRef   `json:"serving"`
	TrafficEnabled  bool          `json:"trafficEnabled"`
	TrafficWrites   int64         `json:"trafficWrites"`
}

func (s *server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	state, err := s.orchestrator.Get(ctx, projectionName)
	if errors.Is(err, aggregatestore.ErrAggregateNotFound) {
		// No rebuild has ever been recorded: a pristine lifecycle.
		state = lifecycle.State{Name: projectionName}
	} else if err != nil {
		s.fail(w, err)
		return
	}

	view := overview{
		Projection:      state.Name,
		CutoverRevision: state.CutoverRevision,
		Allocated:       state.Allocated,
		Policy: policyView{
			Generation:  state.RetirementPolicy.Generation,
			Witnesses:   append([]string{}, state.RetirementPolicy.Witnesses...),
			Unwitnessed: state.RetirementPolicy.Unwitnessed,
		},
	}

	live, err := s.router.Live(ctx, projectionName)
	if err == nil {
		ref := refOf(live)
		view.Live = &ref
	} else if !errors.Is(err, lifecycle.ErrNoLiveVersion) {
		s.fail(w, err)
		return
	}

	attempt := state.Attempt
	if attempt.Phase != lifecycle.PhaseNone {
		attemptRef := refOf(attempt.Target)
		view.Attempt = &attemptView{
			ID:            attempt.ID.String(),
			Phase:         attempt.Phase.String(),
			Reason:        attempt.Reason,
			Target:        attemptRef,
			ClaimStanding: !attempt.Runner.IsNil() && !attempt.Released,
			InitiatedAt:   attempt.InitiatedAt,
			CaughtUpPos:   attempt.CaughtUpPos,
		}

		if !attempt.Runner.IsNil() {
			view.Attempt.Runner = attempt.Runner.String()
		}

		if attempt.Previous.Version != 0 {
			previousRef := refOf(attempt.Previous)
			view.Attempt.Previous = &previousRef
		}
	}

	view.Versions, err = s.versionCards(ctx, live, attempt)
	if err != nil {
		s.fail(w, err)
		return
	}

	view.Run = s.runView()

	if steady, ok := s.serving.serving(); ok {
		ref := refOf(steady)
		view.Serving = &ref
	}

	view.TrafficEnabled, view.TrafficWrites = s.traffic.running()

	s.respond(w, http.StatusOK, view)
}

// versionCards assembles one card per distinct version of interest: the
// attempt's previous version (the rollback target), the live version, and
// the attempt's build target — with their table contents and checkpoints.
func (s *server) versionCards(ctx context.Context, live projection.ID, attempt lifecycle.AttemptState) ([]versionCard, error) {
	type slot struct {
		id   projection.ID
		role string
	}

	slots := []slot{}

	if attempt.Phase != lifecycle.PhaseNone && attempt.Previous.Version != 0 {
		slots = append(slots, slot{attempt.Previous, "previous"})
	}

	if live.Version != 0 {
		slots = append(slots, slot{live, "live"})
	}

	if attempt.Phase != lifecycle.PhaseNone {
		slots = append(slots, slot{attempt.Target, "target"})
	}

	cards := []versionCard{}

	for _, entry := range slots {
		merged := false

		for i := range cards {
			if cards[i].Ref.Version == entry.id.Version {
				cards[i].Roles = append(cards[i].Roles, entry.role)
				merged = true

				break
			}
		}

		if merged {
			continue
		}

		rows, exists, err := s.model.table(ctx, entry.id)
		if err != nil {
			return nil, err
		}

		card := versionCard{
			Roles:    []string{entry.role},
			Ref:      refOf(entry.id),
			Enriched: enriched(entry.id),
			Exists:   exists,
			Rows:     rows,
		}

		if checkpoint, err := s.checkpoints.Load(ctx, entry.id); err == nil {
			card.Checkpoint = &checkpointView{
				Position:   checkpoint.Position,
				UpdatedAt:  checkpoint.UpdatedAt,
				AgeSeconds: time.Since(checkpoint.UpdatedAt).Seconds(),
			}
		} else if !errors.Is(err, checkpointstore.ErrCheckpointNotFound) {
			return nil, err
		}

		cards = append(cards, card)
	}

	return cards, nil
}

func (s *server) runView() runView {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.run == nil {
		return runView{}
	}

	view := runView{Active: !s.run.finished, Finished: s.run.finished}

	if s.run.finished && s.run.err != nil {
		view.Result = s.run.err.Error()
		view.Failed = true
		view.ClaimStanding = errors.Is(s.run.err, lifecycle.ErrClaimStanding)
		view.Displaced = errors.Is(s.run.err, lifecycle.ErrRunnerDisplaced)
	}

	return view
}

// --- accounts ---

type accountsResponse struct {
	ServedBy *versionRef  `json:"servedBy"`
	Rows     []balanceRow `json:"rows"`
}

// handleListAccounts is the application read path: it asks the router which
// version serves reads and queries that version's table — the logical
// cutover in action. Before any first promotion there is nothing to read.
func (s *server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	live, err := s.router.Live(r.Context(), projectionName)
	if errors.Is(err, lifecycle.ErrNoLiveVersion) {
		s.respond(w, http.StatusOK, accountsResponse{Rows: []balanceRow{}})
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}

	rows, exists, err := s.model.table(r.Context(), live)
	if err != nil {
		s.fail(w, err)
		return
	}

	if !exists {
		rows = []balanceRow{}
	}

	ref := refOf(live)
	s.respond(w, http.StatusOK, accountsResponse{ServedBy: &ref, Rows: rows})
}

func (s *server) handleOpenAccount(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Holder  string `json:"holder"`
		Deposit int64  `json:"deposit"`
	}

	if !s.decode(w, r, &request) {
		return
	}

	id := uuid.Must(uuid.NewV4())
	aggregate := s.accounts.New(id)
	account := aggregate.State()

	open, err := account.Open(request.Holder, time.Now())
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err)
		return
	}

	events := []estoria.DomainEvent[Account]{open}

	if request.Deposit > 0 {
		deposit, err := open.ApplyTo(account).Deposit(request.Deposit, time.Now())
		if err != nil {
			s.respondError(w, http.StatusBadRequest, err)
			return
		}

		events = append(events, deposit)
	}

	aggregate.Append(events...)

	if err := s.accounts.Save(r.Context(), aggregate, nil); err != nil {
		s.fail(w, err)
		return
	}

	s.respond(w, http.StatusCreated, map[string]string{"id": id.String()})
}

func (s *server) handleDeposit(w http.ResponseWriter, r *http.Request) {
	s.handleAmountCommand(w, r, Account.Deposit)
}

func (s *server) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	s.handleAmountCommand(w, r, Account.Withdraw)
}

func (s *server) handleAmountCommand(w http.ResponseWriter, r *http.Request, command func(Account, int64, time.Time) (estoria.DomainEvent[Account], error)) {
	id, err := uuid.FromString(r.PathValue("id"))
	if err != nil {
		s.respondError(w, http.StatusBadRequest, fmt.Errorf("invalid account id: %w", err))
		return
	}

	var request struct {
		Amount int64 `json:"amount"`
	}

	if !s.decode(w, r, &request) {
		return
	}

	aggregate, err := s.accounts.Load(r.Context(), id, nil)
	if err != nil {
		s.fail(w, err)
		return
	}

	event, err := command(aggregate.State(), request.Amount, time.Now())
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err)
		return
	}

	aggregate.Append(event)

	if err := s.accounts.Save(r.Context(), aggregate, nil); err != nil {
		s.fail(w, err)
		return
	}

	s.respond(w, http.StatusOK, map[string]int64{"balance": aggregate.State().Balance})
}

// --- lifecycle commands ---

func (s *server) handleBeginRebuild(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Reason string `json:"reason"`
	}

	if !s.decode(w, r, &request) {
		return
	}

	if request.Reason == "" {
		request.Reason = "operator-initiated rebuild"
	}

	handle, err := s.orchestrator.Begin(r.Context(), projectionName, request.Reason)
	if err != nil {
		s.fail(w, err)
		return
	}

	s.startRun(w, handle)
}

func (s *server) handleResumeRebuild(w http.ResponseWriter, r *http.Request) {
	var request struct {
		TakeoverActor  string `json:"takeoverActor"`
		TakeoverReason string `json:"takeoverReason"`
	}

	if !s.decode(w, r, &request) {
		return
	}

	handle, err := s.orchestrator.Resume(r.Context(), projectionName)
	if err != nil {
		s.fail(w, err)
		return
	}

	var opts []lifecycle.RunOption
	if request.TakeoverActor != "" || request.TakeoverReason != "" {
		opts = append(opts, lifecycle.WithTakeover(request.TakeoverActor, request.TakeoverReason))
	}

	s.startRun(w, handle, opts...)
}

// startRun launches the handle's Run on the application context and holds
// the response briefly: a prompt refusal — a standing claim, nothing to run
// — surfaces synchronously, while a run that is still going after the grace
// period is reported accepted and observed through the overview.
func (s *server) startRun(w http.ResponseWriter, handle *lifecycle.Rebuild, opts ...lifecycle.RunOption) {
	s.mu.Lock()

	if s.run != nil && !s.run.finished {
		s.mu.Unlock()
		s.respondError(w, http.StatusConflict, errors.New("a rebuild run is already active in this process"))

		return
	}

	ctx, cancel := context.WithCancel(s.appCtx)
	run := &rebuildRun{handle: handle, cancel: cancel, done: make(chan struct{})}
	s.run = run
	s.mu.Unlock()

	go func() {
		err := handle.Run(ctx, opts...)

		s.mu.Lock()
		run.err = err
		run.finished = true
		s.mu.Unlock()

		if err != nil {
			s.log.Error("rebuild run ended", "error", err)
		} else {
			s.log.Info("rebuild run ended cleanly")
		}

		close(run.done)
		cancel()
	}()

	select {
	case <-run.done:
		if run.err != nil {
			s.lifecycleError(w, run.err)
			return
		}

		s.respond(w, http.StatusOK, map[string]bool{"finished": true})
	case <-time.After(300 * time.Millisecond):
		s.respond(w, http.StatusAccepted, map[string]bool{"running": true})
	}
}

// drain joins the active rebuild run, bounded by ctx. A graceful shutdown
// must wait for the run's wind-down: the wind-down durably releases the
// run's claim, and a process that exits before the release lands leaves a
// standing claim behind — recoverable only by an operator takeover, exactly
// as after a crash.
func (s *server) drain(ctx context.Context) {
	s.mu.Lock()
	run := s.run
	s.mu.Unlock()

	if run == nil {
		return
	}

	select {
	case <-run.done:
	case <-ctx.Done():
		s.log.Warn("shutdown proceeded before the rebuild run wound down; its claim may still be standing")
	}
}

// commandHandle returns the active run's handle when one exists, resuming a
// fresh handle otherwise: rollback, abandonment, and retirement act on the
// durable record and need no certified run.
func (s *server) commandHandle(ctx context.Context) (*lifecycle.Rebuild, error) {
	s.mu.Lock()
	run := s.run
	s.mu.Unlock()

	if run != nil && !run.finished {
		return run.handle, nil
	}

	return s.orchestrator.Resume(ctx, projectionName)
}

func (s *server) handlePromote(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	run := s.run
	s.mu.Unlock()

	if run == nil || run.finished {
		s.respondError(w, http.StatusConflict,
			errors.New("promotion requires the certified run: resume the rebuild in this process first"))

		return
	}

	if err := run.handle.Promote(r.Context()); err != nil {
		s.lifecycleError(w, err)
		return
	}

	s.respond(w, http.StatusOK, map[string]bool{"promoted": true})
}

func (s *server) handleRollback(w http.ResponseWriter, r *http.Request) {
	handle, err := s.commandHandle(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}

	if err := handle.Rollback(r.Context()); err != nil {
		s.lifecycleError(w, err)
		return
	}

	s.respond(w, http.StatusOK, map[string]bool{"rolledBack": true})
}

func (s *server) handleAbandon(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Cause string `json:"cause"`
	}

	if !s.decode(w, r, &request) {
		return
	}

	if request.Cause == "" {
		request.Cause = "operator abandoned the rebuild"
	}

	handle, err := s.commandHandle(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}

	if err := handle.Abandon(r.Context(), request.Cause); err != nil {
		s.lifecycleError(w, err)
		return
	}

	s.respond(w, http.StatusOK, map[string]bool{"abandoned": true})
}

func (s *server) handleRetire(w http.ResponseWriter, r *http.Request) {
	var request struct {
		OverrideActor  string `json:"overrideActor"`
		OverrideReason string `json:"overrideReason"`
	}

	if !s.decode(w, r, &request) {
		return
	}

	handle, err := s.commandHandle(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}

	var opts []lifecycle.RetireOption
	if request.OverrideActor != "" || request.OverrideReason != "" {
		opts = append(opts, lifecycle.WithRetirementOverride(request.OverrideActor, request.OverrideReason))
	}

	if err := handle.Retire(r.Context(), opts...); err != nil {
		s.lifecycleError(w, err)
		return
	}

	s.respond(w, http.StatusOK, map[string]bool{"retired": true})
}

func (s *server) handleSetPolicy(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Witnesses   []string `json:"witnesses"`
		Unwitnessed bool     `json:"unwitnessed"`
		Actor       string   `json:"actor"`
		Reason      string   `json:"reason"`
	}

	if !s.decode(w, r, &request) {
		return
	}

	change := lifecycle.RetirementPolicyChange{
		Witnesses:   request.Witnesses,
		Unwitnessed: request.Unwitnessed,
		Actor:       request.Actor,
		Reason:      request.Reason,
	}

	if err := s.orchestrator.SetRetirementPolicy(r.Context(), projectionName, change); err != nil {
		s.lifecycleError(w, err)
		return
	}

	s.respond(w, http.StatusOK, map[string]bool{"applied": true})
}

func (s *server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled bool `json:"enabled"`
	}

	if !s.decode(w, r, &request) {
		return
	}

	s.traffic.setEnabled(request.Enabled)
	s.respond(w, http.StatusOK, map[string]bool{"enabled": request.Enabled})
}

// --- plumbing ---

func (s *server) decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if r.ContentLength == 0 {
		return true
	}

	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		s.respondError(w, http.StatusBadRequest, fmt.Errorf("decoding request: %w", err))
		return false
	}

	return true
}

func (s *server) respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.Error("encoding response", "error", err)
	}
}

type errorResponse struct {
	Error         string `json:"error"`
	ClaimStanding bool   `json:"claimStanding,omitempty"`
	Displaced     bool   `json:"displaced,omitempty"`
	NotCertified  bool   `json:"notCertified,omitempty"`
	Conflict      bool   `json:"conflict,omitempty"`
}

func (s *server) respondError(w http.ResponseWriter, status int, err error) {
	s.respond(w, status, errorResponse{Error: err.Error()})
}

// lifecycleError maps a lifecycle command's refusal onto the console's
// vocabulary: standing claims and stale certificates are conflicts the
// operator resolves, not server faults.
func (s *server) lifecycleError(w http.ResponseWriter, err error) {
	response := errorResponse{
		Error:         err.Error(),
		ClaimStanding: errors.Is(err, lifecycle.ErrClaimStanding),
		Displaced:     errors.Is(err, lifecycle.ErrRunnerDisplaced),
		NotCertified:  errors.Is(err, lifecycle.ErrNotCertified),
	}

	var mismatch eventstore.StreamVersionMismatchError
	response.Conflict = errors.As(err, &mismatch)

	status := http.StatusConflict
	if !response.ClaimStanding && !response.Displaced && !response.NotCertified && !response.Conflict {
		status = http.StatusBadRequest
	}

	s.respond(w, status, response)
}

func (s *server) fail(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "error", err)
	s.respondError(w, http.StatusInternalServerError, err)
}
