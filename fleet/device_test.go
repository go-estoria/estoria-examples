package main

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore/memory"
	"github.com/go-estoria/estoria/snapshotstore"
	streamsnapshots "github.com/go-estoria/estoria/snapshotstore/eventstream"
	"github.com/go-estoria/estoria/typeid"
	"github.com/gofrs/uuid/v5"
)

// Event-sourced domains are easy to test: given a device, apply an event,
// assert on the resulting state. No storage, no mocks.

// t0 is a fixed UTC base time so entities survive JSON round trips
// byte-for-byte (snapshots and events are marshaled as JSON).
var t0 = time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

func reading(i int, temp, humidity float64) ReadingRecorded {
	return ReadingRecorded{
		At:         t0.Add(time.Duration(i) * 2 * time.Second),
		TempC:      temp,
		Humidity:   humidity,
		BatteryPct: 100 - float64(i)*0.05,
	}
}

func registered() Device {
	return DeviceRegistered{
		Name: "greenhouse-01", Model: "AT-100", Location: "building A", Firmware: "1.0.0",
	}.ApplyTo(NewDevice(uuid.Must(uuid.NewV4())))
}

func TestEventApplication(t *testing.T) {
	t.Parallel()

	t.Run("registration brings the device online with a full battery", func(t *testing.T) {
		t.Parallel()
		device := registered()
		if device.Status != "online" || device.BatteryPct != 100 || device.Name != "greenhouse-01" {
			t.Errorf("device = %+v, want online at 100%% battery", device)
		}
	})

	t.Run("readings accumulate and track min/max", func(t *testing.T) {
		t.Parallel()
		device := registered()
		for i, temp := range []float64{22.0, 19.5, 27.5} {
			device = reading(i, temp, 50).ApplyTo(device)
		}

		if device.ReadingCount != 3 || len(device.Readings) != 3 {
			t.Fatalf("counts = (%d, %d), want (3, 3)", device.ReadingCount, len(device.Readings))
		}
		if device.MinTempC != 19.5 || device.MaxTempC != 27.5 {
			t.Errorf("min/max = %.1f/%.1f, want 19.5/27.5", device.MinTempC, device.MaxTempC)
		}
		if device.LastReading.TempC != 27.5 {
			t.Errorf("last reading = %+v, want 27.5°C", device.LastReading)
		}
	})

	t.Run("the reading ring buffer trims to the newest 60", func(t *testing.T) {
		t.Parallel()
		device := registered()
		for i := 0; i < maxReadings+5; i++ {
			device = reading(i, 20+float64(i)*0.1, 50).ApplyTo(device)
		}

		if len(device.Readings) != maxReadings {
			t.Fatalf("ring buffer length = %d, want %d", len(device.Readings), maxReadings)
		}
		if device.ReadingCount != maxReadings+5 {
			t.Errorf("reading count = %d, want %d (the count survives the trim)", device.ReadingCount, maxReadings+5)
		}
		if oldest := device.Readings[0]; oldest.TempC != 20.5 {
			t.Errorf("oldest retained reading = %+v, want the 6th reading (20.5°C)", oldest)
		}
		if device.MinTempC != 20 {
			t.Errorf("min temp = %.1f, want 20 (min/max consider trimmed readings too)", device.MinTempC)
		}
	})

	t.Run("alerts raise and clear", func(t *testing.T) {
		t.Parallel()
		device := registered()

		device = AlertRaised{Kind: "overheat", Message: "too hot"}.ApplyTo(device)
		if !device.HasAlert("overheat") || device.ActiveAlerts["overheat"] != "too hot" {
			t.Fatalf("alerts = %+v, want an active overheat alert", device.ActiveAlerts)
		}

		device = AlertCleared{Kind: "overheat"}.ApplyTo(device)
		if device.HasAlert("overheat") {
			t.Errorf("alerts = %+v, want the overheat alert cleared", device.ActiveAlerts)
		}
	})

	t.Run("clearing an inactive alert leaves the state unchanged", func(t *testing.T) {
		t.Parallel()
		base := registered()

		device := AlertCleared{Kind: "overheat"}.ApplyTo(base)
		if !reflect.DeepEqual(device, base) {
			t.Errorf("device = %+v, want it unchanged from %+v", device, base)
		}
	})

	t.Run("does not mutate the input device", func(t *testing.T) {
		t.Parallel()
		input := registered()
		input = reading(0, 22, 50).ApplyTo(input)
		input = AlertRaised{Kind: "overheat", Message: "x"}.ApplyTo(input)

		// serialize the input before and after applying more events to it;
		// any shared-slice or shared-map mutation shows up as a difference
		before, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}

		_ = reading(1, 31, 55).ApplyTo(input)
		_ = AlertCleared{Kind: "overheat"}.ApplyTo(input)
		_ = StatusChanged{Status: "offline"}.ApplyTo(input)

		after, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("input device was mutated:\n before %s\n after  %s", before, after)
		}
	})
}

// TestDeviceRoundTrip runs the aggregate lifecycle against estoria's
// in-memory event store: append, save, load, verify.
func TestDeviceRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eventStore, err := memory.NewEventStore()
	if err != nil {
		t.Fatal(err)
	}

	store, err := aggregatestore.New(eventStore, "device", NewDevice,
		aggregatestore.WithEventTypes(deviceEventPrototypes()...))
	if err != nil {
		t.Fatal(err)
	}

	deviceID := uuid.Must(uuid.NewV7())

	agg := store.New(deviceID)
	agg.Append(
		DeviceRegistered{Name: "rooftop-07", Model: "AT-200", Location: "annex", Firmware: "1.2.0"},
		reading(0, 24.5, 48),
		AlertRaised{Kind: "overheat", Message: "temperature 30.2°C exceeds 30°C"},
		StatusChanged{Status: "offline"},
	)
	if err := store.Save(ctx, agg, nil); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load(ctx, deviceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := loaded.Version(); v != 4 {
		t.Fatalf("loaded version = %d, want 4", v)
	}

	device := loaded.State()
	if device.Name != "rooftop-07" || device.Status != "offline" ||
		device.ReadingCount != 1 || !device.HasAlert("overheat") {
		t.Fatalf("loaded device = %+v, want the saved state back", device)
	}
}

// TestSnapshotRoundTrip proves that a SnapshottingStore over the event-stream
// snapshot store produces the same aggregate as a full replay: after enough
// events, the load hydrates from the snapshot stream plus a short replay, and
// the resulting entity is identical to one rebuilt from version 1.
func TestSnapshotRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eventStore, err := memory.NewEventStore()
	if err != nil {
		t.Fatal(err)
	}

	plain, err := aggregatestore.New(eventStore, "device", NewDevice,
		aggregatestore.WithEventTypes(deviceEventPrototypes()...))
	if err != nil {
		t.Fatal(err)
	}

	// the event-stream snapshot store works over any event store, including
	// the in-memory one used here
	snapStore, err := streamsnapshots.New(eventStore)
	if err != nil {
		t.Fatal(err)
	}
	const snapshotEvery = 25
	snapshotting, err := aggregatestore.NewSnapshottingStore(
		plain, snapStore, snapshotstore.EventCountSnapshotPolicy{N: snapshotEvery})
	if err != nil {
		t.Fatal(err)
	}

	deviceID := uuid.Must(uuid.NewV7())

	// register, then save several batches of readings through the
	// snapshotting store so the policy fires along the way
	agg := snapshotting.New(deviceID)
	agg.Append(DeviceRegistered{Name: "coldroom-03", Model: "HygroNode", Location: "yard 3", Firmware: "1.4.0"})
	if err := snapshotting.Save(ctx, agg, nil); err != nil {
		t.Fatal(err)
	}

	const totalReadings = 64
	for batch := 0; batch < totalReadings/8; batch++ {
		agg, err := snapshotting.Load(ctx, deviceID, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 8; i++ {
			n := batch*8 + i
			agg.Append(reading(n, 20+float64(n%10), 40+float64(n%20)))
		}
		if err := snapshotting.Save(ctx, agg, nil); err != nil {
			t.Fatal(err)
		}
	}

	// a snapshot must exist at the latest multiple of snapshotEvery
	snap, err := snapStore.ReadSnapshot(ctx, typeid.New("device", deviceID), snapshotstore.ReadSnapshotOptions{})
	if err != nil {
		t.Fatalf("reading snapshot: %v", err)
	}
	wantSnapVersion := int64((totalReadings + 1) / snapshotEvery * snapshotEvery)
	if snap.AggregateVersion != wantSnapVersion {
		t.Fatalf("snapshot at version %d, want %d", snap.AggregateVersion, wantSnapVersion)
	}

	// hydrating via the snapshot must equal a full replay, entity and version
	fromSnapshot, err := snapshotting.Load(ctx, deviceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := plain.Load(ctx, deviceID, nil)
	if err != nil {
		t.Fatal(err)
	}

	if fromSnapshot.Version() != replayed.Version() || fromSnapshot.Version() != totalReadings+1 {
		t.Fatalf("versions = %d (snapshot) vs %d (replay), want both %d",
			fromSnapshot.Version(), replayed.Version(), totalReadings+1)
	}
	if !reflect.DeepEqual(fromSnapshot.State(), replayed.State()) {
		t.Errorf("snapshot-hydrated entity differs from full replay:\n got %+v\nwant %+v",
			fromSnapshot.State(), replayed.State())
	}
}
