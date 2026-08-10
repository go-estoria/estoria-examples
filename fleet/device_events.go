package main

import (
	"time"

	"github.com/go-estoria/estoria"
)

// Each event below implements estoria.DomainEvent[Device]. The prototypes are
// value-typed (New returns a value, not a pointer); estoria handles making
// them addressable for unmarshaling. ApplyTo implementations are total, pure
// state transitions: they clone the device, apply the change, and return the
// result.
//
// The domain is a pure recorder: it captures what the sensor reported. The
// rules that *decide* what to record — when to raise an alert, which statuses
// exist — live in the simulator, which validates against the state it loaded
// before appending events.

// DeviceRegistered creates a device. A freshly registered device is online
// with a full battery.
type DeviceRegistered struct {
	Name     string `json:"name"`
	Model    string `json:"model"`
	Location string `json:"location"`
	Firmware string `json:"firmware"`
}

func (DeviceRegistered) EventType() string                { return "deviceregistered" }
func (DeviceRegistered) New() estoria.DomainEvent[Device] { return DeviceRegistered{} }

func (e DeviceRegistered) ApplyTo(d Device) Device {
	next := d.clone()
	next.Name = e.Name
	next.Model = e.Model
	next.Location = e.Location
	next.Firmware = e.Firmware
	next.Status = "online"
	next.BatteryPct = 100
	return next
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
func (ReadingRecorded) New() estoria.DomainEvent[Device] { return ReadingRecorded{} }

func (e ReadingRecorded) ApplyTo(d Device) Device {
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
	return next
}

// StatusChanged marks the device online or offline.
type StatusChanged struct {
	Status string `json:"status"`
}

func (StatusChanged) EventType() string                { return "statuschanged" }
func (StatusChanged) New() estoria.DomainEvent[Device] { return StatusChanged{} }

func (e StatusChanged) ApplyTo(d Device) Device {
	next := d.clone()
	next.Status = e.Status
	return next
}

// FirmwareUpdated records a firmware version change.
type FirmwareUpdated struct {
	Version string `json:"version"`
}

func (FirmwareUpdated) EventType() string                { return "firmwareupdated" }
func (FirmwareUpdated) New() estoria.DomainEvent[Device] { return FirmwareUpdated{} }

func (e FirmwareUpdated) ApplyTo(d Device) Device {
	next := d.clone()
	next.Firmware = e.Version
	return next
}

// AlertRaised activates an alert of a given kind (e.g. "overheat").
type AlertRaised struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (AlertRaised) EventType() string                { return "alertraised" }
func (AlertRaised) New() estoria.DomainEvent[Device] { return AlertRaised{} }

func (e AlertRaised) ApplyTo(d Device) Device {
	next := d.clone()
	if next.ActiveAlerts == nil {
		next.ActiveAlerts = make(map[string]string, 1)
	}
	next.ActiveAlerts[e.Kind] = e.Message
	return next
}

// AlertCleared deactivates a previously raised alert. Clearing an alert that
// is not active leaves the state unchanged.
type AlertCleared struct {
	Kind string `json:"kind"`
}

func (AlertCleared) EventType() string                { return "alertcleared" }
func (AlertCleared) New() estoria.DomainEvent[Device] { return AlertCleared{} }

func (e AlertCleared) ApplyTo(d Device) Device {
	next := d.clone()
	delete(next.ActiveAlerts, e.Kind)
	return next
}

// deviceEventPrototypes lists every event type for registration with the
// aggregate store.
func deviceEventPrototypes() []estoria.DomainEvent[Device] {
	return []estoria.DomainEvent[Device]{
		DeviceRegistered{},
		ReadingRecorded{},
		StatusChanged{},
		FirmwareUpdated{},
		AlertRaised{},
		AlertCleared{},
	}
}
