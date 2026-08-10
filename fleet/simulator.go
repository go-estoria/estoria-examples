package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-estoria/estoria"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/gofrs/uuid/v5"
)

// A registry is the in-memory list of known device IDs. It is derived from the
// event store at startup (every stream of type "device" is a device), so there
// is no separate device table to keep in sync — the streams are the registry.
type registry struct {
	mu  sync.RWMutex
	ids []uuid.UUID
}

func (r *registry) add(id uuid.UUID) {
	r.mu.Lock()
	r.ids = append(r.ids, id)
	r.mu.Unlock()
}

func (r *registry) all() []uuid.UUID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]uuid.UUID(nil), r.ids...)
}

func (r *registry) contains(id uuid.UUID) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, known := range r.ids {
		if known == id {
			return true
		}
	}
	return false
}

func (r *registry) count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.ids)
}

// A simulator stands in for a fleet of real sensors: one goroutine per device,
// each appending events to its own stream on a jittered interval. There is no
// external message broker or device gateway — the point is to generate event
// volume, not to model MQTT.
//
// Every tick goes through the full decorated aggregate store: load (a cache
// hit), append, save. That save path is what keeps the cache warm, triggers
// snapshots, and fans out SSE updates — the simulator gets no special access.
type simulator struct {
	store  aggregatestore.Store[Device]
	reg    *registry
	appCtx context.Context

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func newSimulator(appCtx context.Context, store aggregatestore.Store[Device], reg *registry) *simulator {
	return &simulator{store: store, reg: reg, appCtx: appCtx}
}

// start launches one goroutine per registered device. It reports whether the
// simulator transitioned from stopped to running.
func (s *simulator) start() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}

	ctx, cancel := context.WithCancel(s.appCtx)
	s.cancel = cancel
	s.running = true

	for _, id := range s.reg.all() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.runDevice(ctx, id)
		}()
	}

	estoria.GetLogger().Info("simulator started", "devices", s.reg.count())
	return true
}

// stop cancels all device goroutines and waits for them to exit. It reports
// whether the simulator transitioned from running to stopped.
func (s *simulator) stop() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return false
	}

	s.cancel()
	s.wg.Wait()
	s.running = false

	estoria.GetLogger().Info("simulator stopped")
	return true
}

func (s *simulator) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// runDevice is the per-device loop: sleep a jittered 1.5–4s, then load the
// device through the decorated store, decide what happened since the last
// tick, append the resulting events, and save. The device stream is only ever
// written by this goroutine, so version conflicts are not expected; any error
// is logged and the loop continues.
func (s *simulator) runDevice(ctx context.Context, id uuid.UUID) {
	log := estoria.GetLogger().With("device_id", id)

	// each device drifts around its own baseline temperature (20–29°C);
	// warmer baselines cross the simulator's 30°C alert threshold now and then
	baseline := 20 + 9*float64(id[15])/255

	// walker state seeded from the device's persisted last reading
	temp, humidity := baseline, 50.0
	if agg, err := s.store.Load(ctx, id, nil); err == nil {
		if d := agg.State(); d.ReadingCount > 0 {
			temp, humidity = d.LastReading.TempC, d.LastReading.Humidity
		}
	}

	offlineTicks := 0

	for {
		jitter := 1500*time.Millisecond + rand.N(2500*time.Millisecond)
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter):
		}

		agg, err := s.store.Load(ctx, id, nil)
		if err != nil {
			log.Error("simulator: loading device", "error", err)
			continue
		}
		device := agg.State()

		events := s.tick(&temp, &humidity, &offlineTicks, baseline, device)
		if len(events) == 0 {
			continue
		}

		agg.Append(events...)
		if err := s.store.Save(ctx, agg, nil); err != nil {
			log.Error("simulator: saving device", "error", err)
		}
	}
}

// tick advances the random walk one step and derives the events for it.
// The alert rule (overheat above 30°C, cleared below 28°C) lives here, not in
// the domain: the aggregate is a pure recorder of what the fleet reported.
func (s *simulator) tick(temp, humidity *float64, offlineTicks *int, baseline float64, device Device) []estoria.DomainEvent[Device] {
	// offline devices skip readings until they come back
	if device.Status == "offline" {
		*offlineTicks--
		if *offlineTicks <= 0 {
			return []estoria.DomainEvent[Device]{StatusChanged{Status: "online"}}
		}
		return nil
	}

	// ~1% chance per tick of dropping offline for a few ticks
	if rand.Float64() < 0.01 {
		*offlineTicks = 2 + rand.IntN(4)
		return []estoria.DomainEvent[Device]{StatusChanged{Status: "offline"}}
	}

	// temperature: bounded random walk with gentle pull toward the baseline
	*temp += (baseline-*temp)*0.03 + (rand.Float64()-0.5)*1.4
	*temp = min(max(*temp, 16), 36)

	// humidity: bounded random walk
	*humidity += (rand.Float64() - 0.5) * 3
	*humidity = min(max(*humidity, 30), 70)

	// battery: slow drain with an occasional recharge to full
	battery := device.BatteryPct - 0.01 - rand.Float64()*0.04
	if battery < 3 || (battery < 20 && rand.Float64() < 0.02) {
		battery = 100
	}

	events := []estoria.DomainEvent[Device]{ReadingRecorded{
		At:         time.Now().UTC(),
		TempC:      round1(*temp),
		Humidity:   round1(*humidity),
		BatteryPct: round1(battery),
	}}

	switch {
	case *temp > 30 && !device.HasAlert("overheat"):
		events = append(events, AlertRaised{
			Kind:    "overheat",
			Message: fmt.Sprintf("temperature %.1f°C exceeds 30°C", *temp),
		})
	case *temp < 28 && device.HasAlert("overheat"):
		events = append(events, AlertCleared{Kind: "overheat"})
	}

	// rare firmware update
	if rand.Float64() < 0.002 {
		events = append(events, FirmwareUpdated{Version: bumpPatch(device.Firmware)})
	}

	return events
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

// bumpPatch increments the last numeric segment of a dotted version string.
func bumpPatch(version string) string {
	parts := strings.Split(version, ".")
	if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
		parts[len(parts)-1] = strconv.Itoa(n + 1)
	}
	return strings.Join(parts, ".")
}

// Device name generation: enough variety that a 50-device fleet still reads
// like a plausible facility map.

var (
	sitePrefixes = []string{
		"greenhouse", "rooftop", "warehouse", "coldroom", "lobby", "server-rack",
		"loading-dock", "atrium", "basement", "lab", "kiln-room", "archive",
	}
	deviceModels = []string{
		"AT-100", "AT-200", "ThermoPro X", "SensorMax 2", "EnviroSense Lite", "HygroNode",
	}
	siteLocations = []string{
		"building A", "building B", "north wing", "south wing", "annex", "yard 3",
	}
)

// registerDevices creates count new devices, numbered after the existing
// fleet, and adds them to the registry. Each registration is a normal save
// through the decorated store: one DeviceRegistered event per device stream.
func registerDevices(ctx context.Context, store aggregatestore.Store[Device], reg *registry, count int) error {
	for i := 0; i < count; i++ {
		n := reg.count() + 1
		event := DeviceRegistered{
			Name:     fmt.Sprintf("%s-%02d", sitePrefixes[rand.IntN(len(sitePrefixes))], n),
			Model:    deviceModels[rand.IntN(len(deviceModels))],
			Location: siteLocations[rand.IntN(len(siteLocations))],
			Firmware: fmt.Sprintf("1.%d.0", rand.IntN(6)),
		}

		// v7 UUIDs are k-sortable, so device IDs sort by registration time
		agg := store.New(uuid.Must(uuid.NewV7()))
		agg.Append(event)
		if err := store.Save(ctx, agg, nil); err != nil {
			return fmt.Errorf("saving device %q: %w", event.Name, err)
		}

		reg.add(agg.ID().UUID)
	}
	return nil
}
