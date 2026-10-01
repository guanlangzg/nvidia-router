package compat

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
)

type ReasoningLevel string

const (
	ReasoningNone    ReasoningLevel = "none"
	ReasoningAuto    ReasoningLevel = "auto"
	ReasoningMinimal ReasoningLevel = "minimal"
	ReasoningLow     ReasoningLevel = "low"
	ReasoningMedium  ReasoningLevel = "medium"
	ReasoningHigh    ReasoningLevel = "high"
	ReasoningXHigh   ReasoningLevel = "xhigh"
	ReasoningMax     ReasoningLevel = "max"
)

var ErrAmbiguousReasoning = errors.New("reasoning aliases disagree")

// Reasoning handling is pass-through: whatever alias the client sent reaches
// the upstream untouched. ParseReasoning exists purely so the request log can
// record what the caller asked for and so the Requirements flag can carry the
// on/off intent; a parse failure never rejects a request and the per-model
// profile below is advisory metadata, never a rewrite authority.

var reasoningBudgets = map[ReasoningLevel]int{
	ReasoningNone:    0,
	ReasoningAuto:    -1,
	ReasoningMinimal: 512,
	ReasoningLow:     1024,
	ReasoningMedium:  8192,
	ReasoningHigh:    24576,
	ReasoningXHigh:   32768,
	ReasoningMax:     128000,
}

var reasoningAliases = map[string]ReasoningLevel{
	"none": ReasoningNone, "off": ReasoningNone, "disabled": ReasoningNone,
	"no_think": ReasoningNone, "no-think": ReasoningNone,
	"auto": ReasoningAuto, "default": ReasoningAuto, "on": ReasoningAuto,
	"minimal": ReasoningMinimal, "tiny": ReasoningMinimal,
	"low":    ReasoningLow,
	"medium": ReasoningMedium, "med": ReasoningMedium,
	"high":  ReasoningHigh,
	"xhigh": ReasoningXHigh, "very_high": ReasoningXHigh, "very-high": ReasoningXHigh,
	"max": ReasoningMax, "ultra": ReasoningMax,
}

type ReasoningSpec struct {
	Requested bool
	Level     ReasoningLevel
	Budget    int
	HasBudget bool
	Source    string
}

// RequiresReasoning reports whether the caller asked the model to actually spend
// reasoning tokens. Requested alone does not mean that: reasoning_effort:"none",
// thinking:false and thinking:{"type":"disabled"} all parse as Requested because
// the caller did name the parameter, yet they ask for reasoning to stay off —
// something a model without the capability already satisfies.
func (s ReasoningSpec) RequiresReasoning() bool {
	return s.Requested && s.Level != ReasoningNone
}

type ReasoningProfile struct {
	Supported      bool
	Levels         []ReasoningLevel
	MinBudget      int
	MaxBudget      int
	ZeroAllowed    bool
	DynamicAllowed bool
	WireFormat     string
	// AdvisoryLevels marks upstreams that accept an effort string but do not act
	// on its magnitude. Advisory metadata only: nothing on the request path
	// normalises levels anymore.
	AdvisoryLevels bool
}

func ParseReasoning(fields map[string]json.RawMessage) (ReasoningSpec, error) {
	aliases := []struct {
		name  string
		parse func(json.RawMessage, string) (ReasoningSpec, error)
	}{
		{name: "reasoning_effort", parse: parseReasoningEffort},
		{name: "reasoning", parse: parseReasoningObject},
		{name: "thinking", parse: parseThinking},
	}
	var result ReasoningSpec
	for _, alias := range aliases {
		raw, ok := fields[alias.name]
		if !ok || isNull(raw) {
			continue
		}
		spec, err := alias.parse(raw, alias.name)
		if err != nil {
			return ReasoningSpec{}, err
		}
		if !spec.Requested {
			continue
		}
		if !result.Requested {
			result = spec
			continue
		}
		if reasoningSpecsConflict(result, spec) {
			return ReasoningSpec{}, ErrAmbiguousReasoning
		}
		if result.Source == "" {
			result.Source = spec.Source
		}
		if !result.HasBudget && spec.HasBudget {
			result.Budget, result.HasBudget = spec.Budget, true
		}
	}
	return result, nil
}

func parseReasoningEffort(raw json.RawMessage, source string) (ReasoningSpec, error) {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ReasoningSpec{}, invalid("invalid_parameter", source, "The reasoning effort must be a string.")
	}
	level, err := parseFlexibleLevel(value, source)
	if err != nil {
		return ReasoningSpec{}, err
	}
	return ReasoningSpec{Requested: true, Level: level, Budget: budgetForLevel(level), Source: source}, nil
}

func parseReasoningObject(raw json.RawMessage, source string) (ReasoningSpec, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return parseReasoningEffort(raw, source)
	}
	return parseReasoningFields(object, source, false)
}

func parseThinking(raw json.RawMessage, source string) (ReasoningSpec, error) {
	var value bool
	if json.Unmarshal(raw, &value) == nil {
		level := ReasoningAuto
		if !value {
			level = ReasoningNone
		}
		return ReasoningSpec{Requested: true, Level: level, Budget: budgetForLevel(level), Source: source}, nil
	}
	var valueString string
	if json.Unmarshal(raw, &valueString) == nil {
		level, err := parseFlexibleLevel(valueString, source)
		if err != nil {
			return ReasoningSpec{}, err
		}
		return ReasoningSpec{Requested: true, Level: level, Budget: budgetForLevel(level), Source: source}, nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return ReasoningSpec{}, invalid("invalid_parameter", source, "The thinking parameter must be an object, boolean, or string.")
	}
	return parseReasoningFields(object, source, true)
}

func parseReasoningFields(object map[string]json.RawMessage, source string, thinking bool) (ReasoningSpec, error) {
	level := ReasoningLevel("")
	enabled := false
	var err error
	for _, name := range []string{"effort", "level", "reasoning_effort"} {
		if raw, ok := object[name]; ok && !isNull(raw) {
			value, valid := stringValueOK(raw)
			if !valid {
				return ReasoningSpec{}, invalid("invalid_parameter", source+"."+name, "The reasoning level must be a string.")
			}
			level, err = parseFlexibleLevel(value, source+"."+name)
			if err != nil {
				return ReasoningSpec{}, err
			}
			break
		}
	}
	if rawType, ok := object["type"]; ok && !isNull(rawType) {
		typeName, valid := stringValueOK(rawType)
		if !valid || (typeName != "enabled" && typeName != "disabled") {
			return ReasoningSpec{}, invalid("invalid_parameter", source+".type", "The reasoning type must be enabled or disabled.")
		}
		if typeName == "disabled" {
			level = ReasoningNone
		}
		if typeName == "enabled" && level == "" {
			enabled = true
		}
	}
	budget, hasBudget, err := readBudget(object, source)
	if err != nil {
		return ReasoningSpec{}, err
	}
	if level == "" && hasBudget {
		level = levelForBudget(budget)
	}
	if level == "" && enabled {
		level = ReasoningAuto
	}
	if level == "" {
		if thinking {
			return ReasoningSpec{}, invalid("invalid_parameter", source, "The thinking parameter must include a type, effort, level, or budget_tokens.")
		}
		return ReasoningSpec{}, invalid("invalid_parameter", source, "The reasoning parameter must include an effort or budget_tokens.")
	}
	if !hasBudget {
		budget = budgetForLevel(level)
	}
	return ReasoningSpec{Requested: true, Level: level, Budget: budget, HasBudget: hasBudget, Source: source}, nil
}

func readBudget(object map[string]json.RawMessage, source string) (int, bool, error) {
	for _, name := range []string{"budget_tokens", "budget"} {
		raw, ok := object[name]
		if !ok || isNull(raw) {
			continue
		}
		var value int64
		if json.Unmarshal(raw, &value) != nil || value < 0 || value > math.MaxInt32 {
			return 0, false, invalid("invalid_parameter", source+"."+name, "The reasoning budget must be a non-negative integer.")
		}
		return int(value), true, nil
	}
	return 0, false, nil
}

func reasoningSpecsConflict(left, right ReasoningSpec) bool {
	if left.Level != right.Level {
		return true
	}
	return left.HasBudget && right.HasBudget && left.Budget != right.Budget
}

func parseFlexibleLevel(value, param string) (ReasoningLevel, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if level, ok := reasoningAliases[trimmed]; ok {
		return level, nil
	}
	if trimmed == "" || strings.ContainsAny(trimmed, " \t\r\n\x00") || len(trimmed) > 64 {
		return "", invalid("invalid_parameter", param, "The reasoning level must be a non-empty string.")
	}
	return ReasoningLevel(trimmed), nil
}

// AvailableLevels returns the profile's usable levels in ascending budget
// order, dropping "none" when the profile forbids it. Exported so the catalog
// can validate at startup that a reasoning model can express at least one
// level (the llama shape — levels=[none] with zero_allowed=false — yields an
// empty slice and is worth an operator warning even though the request path
// no longer consults the profile).
func AvailableLevels(profile ReasoningProfile) []ReasoningLevel {
	levels := append([]ReasoningLevel(nil), profile.Levels...)
	if len(levels) == 0 {
		levels = []ReasoningLevel{ReasoningNone, ReasoningAuto, ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh, ReasoningMax}
	}
	seen := make(map[ReasoningLevel]struct{}, len(levels))
	result := make([]ReasoningLevel, 0, len(levels))
	for _, level := range levels {
		if _, ok := reasoningBudgets[level]; !ok || level == ReasoningNone && !profile.ZeroAllowed {
			continue
		}
		if _, ok := seen[level]; ok {
			continue
		}
		seen[level] = struct{}{}
		result = append(result, level)
	}
	return result
}

func levelForBudget(value int) ReasoningLevel {
	if value == -1 {
		return ReasoningAuto
	}
	best := ReasoningNone
	bestDistance := int64(math.MaxInt64)
	for level, budget := range reasoningBudgets {
		if level == ReasoningAuto {
			continue
		}
		distance := int64(absInt(value - budget))
		if distance < bestDistance || distance == bestDistance && budget < budgetForLevel(best) {
			best, bestDistance = level, distance
		}
	}
	return best
}

func budgetForLevel(level ReasoningLevel) int {
	return reasoningBudgets[level]
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func stringValueOK(raw json.RawMessage) (string, bool) {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}
