package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Model is one entry of the model catalog. Picker is the name the desktop
// app sends for this model; it may be empty, in which case the model is only
// reachable by its id.
type Model struct {
	ID         string  `json:"id"`
	Picker     string  `json:"picker"`
	Label      string  `json:"label"`
	BehavesAs  string  `json:"behaves_as"`
	MaxEffort  string  `json:"max_effort"`
	Supports1M bool    `json:"supports_1m"`
	Available  bool    `json:"available"`
	Context    int     `json:"context"`
	Pricing    Pricing `json:"pricing"`
}

type Pricing struct {
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	CacheRead float64 `json:"cache_read"`
}

// Registry is the loaded model catalog. It is read-only after LoadRegistry,
// so concurrent readers need no locking.
type Registry struct {
	models   []Model
	byID     map[string]int
	byPicker map[string]int
}

// LoadRegistry reads a models.json file.
func LoadRegistry(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("models: read %s: %w", path, err)
	}
	var file struct {
		Models []Model `json:"models"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("models: parse %s: %w", path, err)
	}

	r := &Registry{
		models:   file.Models,
		byID:     make(map[string]int, len(file.Models)),
		byPicker: make(map[string]int, len(file.Models)),
	}
	for i, m := range file.Models {
		r.byID[strings.ToLower(m.ID)] = i
		if m.Picker != "" {
			r.byPicker[strings.ToLower(m.Picker)] = i
		}
	}
	return r, nil
}

// Native returns the model whose id matches id, ignoring case.
func (r *Registry) Native(id string) (Model, bool) {
	i, ok := r.byID[strings.ToLower(id)]
	if !ok {
		return Model{}, false
	}
	return r.models[i], true
}

// All returns every model in file order, including unavailable ones.
func (r *Registry) All() []Model {
	out := make([]Model, len(r.models))
	copy(out, r.models)
	return out
}

// Available returns the usable models in file order.
func (r *Registry) Available() []Model {
	out := make([]Model, 0, len(r.models))
	for _, m := range r.models {
		if m.Available {
			out = append(out, m)
		}
	}
	return out
}

// Resolve maps a model name sent by the desktop app to the upstream id.
// The first matching rule wins: alias, picker, background, native, default.
// Both the input and the result are case-insensitive; the result is always
// lowercase.
func (r *Registry) Resolve(model string, cfg ModelsConfig) (target, reason string) {
	in := strings.ToLower(strings.TrimSpace(model))
	// The desktop app appends "[1m]" to an id it runs with the 1M context
	// beta; it is a client-side marker the gateway never sees.
	in = strings.TrimSuffix(in, "[1m]")
	if in == "" {
		return strings.ToLower(cfg.Default), "default"
	}

	for name, id := range cfg.Aliases {
		if strings.ToLower(name) == in {
			return strings.ToLower(id), "alias"
		}
	}

	if m, ok := r.picker(in); ok {
		return strings.ToLower(m.ID), "picker"
	}

	if strings.HasPrefix(in, "claude-haiku-") {
		return strings.ToLower(cfg.Background), "background"
	}

	if m, ok := r.Native(in); ok {
		return strings.ToLower(m.ID), "native"
	}

	return strings.ToLower(cfg.Default), "default"
}

// picker matches an explicit picker name, or the conventional claude-<id>
// form for a native model.
func (r *Registry) picker(in string) (Model, bool) {
	if i, ok := r.byPicker[in]; ok {
		return r.models[i], true
	}
	if rest, ok := strings.CutPrefix(in, "claude-"); ok {
		return r.Native(rest)
	}
	return Model{}, false
}
