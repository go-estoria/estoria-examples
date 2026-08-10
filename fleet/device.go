package main

import (
	"time"

	"github.com/gofrs/uuid/v5"
)

// maxReadings is the size of the per-device reading ring buffer. The full
// reading history lives in the event stream; the entity keeps only enough
// recent readings to draw charts.
const maxReadings = 60

// A Reading is a single sensor measurement.
type Reading struct {
	At       time.Time `json:"at"`
	TempC    float64   `json:"tempC"`
	Humidity float64   `json:"humidity"`
}

// A Device is the aggregate root for one sensor. Its state is derived entirely
// by applying events; it is never mutated directly. Unlike the streams in most
// CRUD-shaped examples, a device stream grows without bound (a reading every
// few seconds, forever), which is exactly what makes snapshotting and caching
// worth demonstrating.
type Device struct {
	ID           uuid.UUID         `json:"id"`
	Name         string            `json:"name"`
	Model        string            `json:"model"`
	Location     string            `json:"location"`
	Firmware     string            `json:"firmware"`
	Status       string            `json:"status"`
	BatteryPct   float64           `json:"batteryPct"`
	LastReading  Reading           `json:"lastReading"`
	Readings     []Reading         `json:"readings"`
	ReadingCount int64             `json:"readingCount"`
	MinTempC     float64           `json:"minTempC"`
	MaxTempC     float64           `json:"maxTempC"`
	ActiveAlerts map[string]string `json:"activeAlerts,omitempty"`
}

// NewDevice is the estoria.StateFactory for Device aggregates.
func NewDevice(id uuid.UUID) Device {
	return Device{ID: id}
}

// clone returns a deep copy of the device so that ApplyTo implementations can
// return new state without mutating the slice and map shared with previous
// versions.
func (d Device) clone() Device {
	c := d
	c.Readings = make([]Reading, len(d.Readings))
	copy(c.Readings, d.Readings)
	if d.ActiveAlerts != nil {
		c.ActiveAlerts = make(map[string]string, len(d.ActiveAlerts))
		for kind, message := range d.ActiveAlerts {
			c.ActiveAlerts[kind] = message
		}
	}
	return c
}

// Registered reports whether the device's registration event has been applied.
func (d Device) Registered() bool {
	return d.Name != ""
}

// HasAlert reports whether an alert of the given kind is active.
func (d Device) HasAlert(kind string) bool {
	_, ok := d.ActiveAlerts[kind]
	return ok
}
