// Package pricing loads the rate table shared by M2's pre-merge estimate and M4's live
// attribution.
//
// One table for both is a deliberate constraint, not a convenience. A pre-merge estimate
// and a post-deploy measurement are only comparable if they are computed from identical
// rates; two sources would produce two numbers that look comparable and are not.
package pricing

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

type Table struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

type Metadata struct {
	Name string `json:"name"`
	// Recorded in every verdict so an old pull-request comment can always be traced
	// back to the rates that produced it, even after the table changes.
	Version     int    `json:"version"`
	Source      string `json:"source"`
	RetrievedAt string `json:"retrievedAt"`
}

type Spec struct {
	Currency        string          `json:"currency"`
	HoursPerMonth   float64         `json:"hoursPerMonth"`
	Rates           Rates           `json:"rates"`
	Defaults        Defaults        `json:"defaults"`
	MissingRequests MissingRequests `json:"missingRequests"`
}

type Rate struct {
	Unit  string  `json:"unit"`
	Price float64 `json:"price"`
}

type Rates struct {
	CPU     Rate `json:"cpu"`
	Memory  Rate `json:"memory"`
	Storage Rate `json:"storage"`
}

type Defaults struct {
	UnknownResource Rate `json:"unknownResource"`
}

// MissingRequests is what the gate assumes for a container that declares no requests.
//
// A container without requests is not free — it is unbounded, and the scheduler will
// place it anywhere. Pricing it at zero would mean the cheapest possible manifest is
// also the most dangerous one, and the cost gate would reward exactly the practice the
// policy gate exists to forbid.
type MissingRequests struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	Flag   bool   `json:"flag"`
}

func Load(path string) (*Table, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pricing table: %w", err)
	}
	var t Table
	if err := yaml.UnmarshalStrict(raw, &t); err != nil {
		return nil, fmt.Errorf("parse pricing table %s: %w", path, err)
	}
	if err := t.validate(); err != nil {
		return nil, fmt.Errorf("invalid pricing table %s: %w", path, err)
	}
	return &t, nil
}

func (t *Table) validate() error {
	if t.Spec.HoursPerMonth <= 0 {
		return fmt.Errorf("spec.hoursPerMonth must be > 0")
	}
	// Zero rates are permitted (a resource may genuinely be unpriced) but negative
	// rates would make added capacity reduce cost and invert the gate's decision.
	if t.Spec.Rates.CPU.Price < 0 || t.Spec.Rates.Memory.Price < 0 || t.Spec.Rates.Storage.Price < 0 {
		return fmt.Errorf("rates must not be negative")
	}
	if t.Spec.Currency == "" {
		return fmt.Errorf("spec.currency is required")
	}
	return nil
}

// HourlyUSD prices one hour of the given reserved capacity.
func (t *Table) HourlyUSD(cpuCores, memoryGiB, storageGiB float64) float64 {
	return cpuCores*t.Spec.Rates.CPU.Price +
		memoryGiB*t.Spec.Rates.Memory.Price +
		storageGiB*t.Spec.Rates.Storage.Price
}

// MonthlyUSD prices a month of the given reserved capacity. Monthly is the unit the
// gate reports in because budgets are set monthly; hourly figures invite a mental
// arithmetic error in exactly the moment someone is deciding whether to merge.
func (t *Table) MonthlyUSD(cpuCores, memoryGiB, storageGiB float64) float64 {
	return t.HourlyUSD(cpuCores, memoryGiB, storageGiB) * t.Spec.HoursPerMonth
}
