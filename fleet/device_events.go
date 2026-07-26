package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-estoria/estoria"
)

// Each event below implements estoria.EntityEvent[Device]. The prototypes are
// value-typed (New returns a value, not a pointer); estoria handles making
// them addressable for unmarshaling. ApplyTo implementations are pure state
// transitions: they clone the device, apply the change, and return the result.
//
// The domain is a pure recorder: it captures what the sensor reported and
// enforces internal consistency (no duplicate alerts, no unknown statuses),
// but the rules that *decide* when to raise an alert live in the simulator.

// DeviceRegistered creates a device. A freshly registered device is online
// with a full battery.
type DeviceRegistered struct {
	Name     string `json:"name"`
	Model    string `json:"model"`
	Location string `json:"location"`
	Firmware string `json:"firmware"`
}

func (DeviceRegistered) EventType() string                { return "deviceregistered" }
func (DeviceRegistered) New() estoria.EntityEvent[Device] { return DeviceRegistered{} }
func (e DeviceRegistered) ApplyTo(_ context.Context, d Device) (Device, error) {
	if d.Registered() {
		return d, fmt.Errorf("device %s is already registered", d.ID)
	}

	next := d.clone()
	next.Name = e.Name
	next.Model = e.Model
	next.Location = e.Location
	next.Firmware = e.Firmware
	next.Status = "online"
	next.BatteryPct = 100
	return next, nil
}

// ReadingRecorded captures one sensor measurement. This is the event that
// dominates a device's stream: one every few seconds, indefinitely. The
// timestamp is part of the event payload because ApplyTo sees only event data,
// not stream metadata.
type ReadingRecorded struct {
	At         time.Time `json:"at"`
	TempC      float64   `json:"tempC"`
	Humidity   float64   `json:"humidity"`
	BatteryPct float64   `json:"batteryPct"`
}

func (ReadingRecorded) EventType() string                { return "readingrecorded" }
func (ReadingRecorded) New() estoria.EntityEvent[Device] { return ReadingRecorded{} }
func (e ReadingRecorded) ApplyTo(_ context.Context, d Device) (Device, error) {
	next := d.clone()
	next.BatteryPct = e.BatteryPct
	next.LastReading = Reading{At: e.At, TempC: e.TempC, Humidity: e.Humidity}

	// ring buffer: keep only the most recent maxReadings readings
	next.Readings = append(next.Readings, next.LastReading)
	if n := len(next.Readings); n > maxReadings {
		next.Readings = next.Readings[n-maxReadings:]
	}

	if next.ReadingCount == 0 {
		next.MinTempC, next.MaxTempC = e.TempC, e.TempC
	} else {
		next.MinTempC = min(next.MinTempC, e.TempC)
		next.MaxTempC = max(next.MaxTempC, e.TempC)
	}
	next.ReadingCount++
	return next, nil
}

// StatusChanged marks the device online or offline.
type StatusChanged struct {
	Status string `json:"status"`
}

func (StatusChanged) EventType() string                { return "statuschanged" }
func (StatusChanged) New() estoria.EntityEvent[Device] { return StatusChanged{} }
func (e StatusChanged) ApplyTo(_ context.Context, d Device) (Device, error) {
	if e.Status != "online" && e.Status != "offline" {
		return d, fmt.Errorf("unknown status %q", e.Status)
	}

	next := d.clone()
	next.Status = e.Status
	return next, nil
}

// FirmwareUpdated records a firmware version change.
type FirmwareUpdated struct {
	Version string `json:"version"`
}

func (FirmwareUpdated) EventType() string                { return "firmwareupdated" }
func (FirmwareUpdated) New() estoria.EntityEvent[Device] { return FirmwareUpdated{} }
func (e FirmwareUpdated) ApplyTo(_ context.Context, d Device) (Device, error) {
	if e.Version == "" {
		return d, errors.New("firmware version is required")
	}

	next := d.clone()
	next.Firmware = e.Version
	return next, nil
}

// AlertRaised activates an alert of a given kind (e.g. "overheat").
type AlertRaised struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (AlertRaised) EventType() string                { return "alertraised" }
func (AlertRaised) New() estoria.EntityEvent[Device] { return AlertRaised{} }
func (e AlertRaised) ApplyTo(_ context.Context, d Device) (Device, error) {
	if e.Kind == "" {
		return d, errors.New("alert kind is required")
	}
	if d.HasAlert(e.Kind) {
		return d, fmt.Errorf("alert %q is already active", e.Kind)
	}

	next := d.clone()
	if next.ActiveAlerts == nil {
		next.ActiveAlerts = make(map[string]string, 1)
	}
	next.ActiveAlerts[e.Kind] = e.Message
	return next, nil
}

// AlertCleared deactivates a previously raised alert.
type AlertCleared struct {
	Kind string `json:"kind"`
}

func (AlertCleared) EventType() string                { return "alertcleared" }
func (AlertCleared) New() estoria.EntityEvent[Device] { return AlertCleared{} }
func (e AlertCleared) ApplyTo(_ context.Context, d Device) (Device, error) {
	if !d.HasAlert(e.Kind) {
		return d, fmt.Errorf("alert %q is not active", e.Kind)
	}

	next := d.clone()
	delete(next.ActiveAlerts, e.Kind)
	return next, nil
}

// deviceEventPrototypes lists every event type for registration with the
// aggregate store.
func deviceEventPrototypes() []estoria.EntityEvent[Device] {
	return []estoria.EntityEvent[Device]{
		DeviceRegistered{},
		ReadingRecorded{},
		StatusChanged{},
		FirmwareUpdated{},
		AlertRaised{},
		AlertCleared{},
	}
}
