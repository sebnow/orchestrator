package server

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// An agent's models are acceptable models, most preferred first; a
// daemon advertises the models it provides through the label and the
// fact protocol.FactModels
// (docs/adr/2026-10-10-agent-models-and-capacity.md). The orchestrator
// keeps no table of equivalent names: an entry matches a model a daemon
// advertises by its text alone.

// modelSeparator separates the models in the value of a models label or
// fact, since a label's value holds no comma or space.
const modelSeparator = ";"

// validModel reports whether model can be matched against the models a
// daemon advertises: 1 to 255 printable ASCII characters other than
// space, ',', '=' and ';', as a label's value holds them.
func validModel(model string) bool {
	return validLabelValue(model) && !strings.Contains(model, modelSeparator)
}

// validateModels checks that models are models an agent can name, each
// once.
func validateModels(models []string) error {
	for idx, model := range models {
		if !validModel(model) {
			return fmt.Errorf("models: %q must be 1 to %d printable characters other than space, ',', '=' and ';'", model, maxLabelValue)
		}
		if slices.Contains(models[:idx], model) {
			return fmt.Errorf("models: %q is given twice", model)
		}
	}
	return nil
}

// advertisedModels returns the models a daemon provides: those its
// owner's models label names and those its adapter reports in its models
// fact, the label's first. Unlike other labels, the label adds to the
// fact rather than replacing it.
func advertisedModels(facts, labels Labels) []string {
	var models []string
	for _, value := range []string{labels[protocol.FactModels], facts[protocol.FactModels]} {
		for model := range strings.SplitSeq(value, modelSeparator) {
			if model != "" && !slices.Contains(models, model) {
				models = append(models, model)
			}
		}
	}
	return models
}

// serves reports whether a daemon running harness and advertising
// models can run entry, an agent's model: it advertises entry itself,
// or entry is qualified by the daemon's harness, as harness:model, and
// the daemon advertises model. An entry is not split at its first ':'
// unless that matches the harness, because model identifiers such as
// Bedrock's and Ollama's hold colons of their own.
func serves(harness string, models []string, entry string) bool {
	if slices.Contains(models, entry) {
		return true
	}
	if harness == "" {
		return false
	}
	model, qualified := strings.CutPrefix(entry, harness+":")
	return qualified && slices.Contains(models, model)
}

// alternatives words models as a choice: "a", "a or b", "a, b or c".
func alternatives(models []string) string {
	if len(models) < 2 {
		return strings.Join(models, "")
	}
	return strings.Join(models[:len(models)-1], ", ") + " or " + models[len(models)-1]
}
