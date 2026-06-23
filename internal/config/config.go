// Package config loads scenarios and the run configuration from JSON files and
// applies defaults. It is the boundary between the CLI flags/files and the typed
// Scenario the runner consumes.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"voicechaos/internal/baseline"
	"voicechaos/internal/script"
)

// LoadScenario reads and validates a Scenario from a JSON file.
func LoadScenario(path string) (*script.Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sc script.Scenario
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sc); err != nil {
		return nil, fmt.Errorf("config: parse scenario %s: %w", path, err)
	}
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	return &sc, nil
}

// LoadBudget reads a Budget from a JSON file, falling back to the default budget
// when path is empty.
func LoadBudget(path string) (baseline.Budget, error) {
	if path == "" {
		return baseline.DefaultBudget, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return baseline.Budget{}, err
	}
	b := baseline.DefaultBudget
	if err := json.Unmarshal(data, &b); err != nil {
		return baseline.Budget{}, fmt.Errorf("config: parse budget %s: %w", path, err)
	}
	return b, nil
}

// WriteScenario writes a scenario to path as indented JSON (used to emit example
// scenarios).
func WriteScenario(path string, sc *script.Scenario) error {
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
