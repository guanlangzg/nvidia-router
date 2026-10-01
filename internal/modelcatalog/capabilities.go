package modelcatalog

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrModelNotFound         = errors.New("model not found")
	ErrModelKindMismatch     = errors.New("model kind does not match endpoint")
	ErrCapabilityUnsupported = errors.New("model capability is not supported")
	ErrCapabilityUnverified  = errors.New("model capability is not verified")
	ErrManualTestRequired    = errors.New("successful manual test is required")
	ErrNVIDIAKeyRequired     = errors.New("an NVIDIA key is required")
	// ErrUpstreamUnreachable marks a probe that never got a usable answer from
	// the upstream: a transport failure, a missing body, a throttled or 5xx
	// answer, or an empty completion. It wraps ErrManualTestRequired so every
	// existing caller keeps its behaviour, while the retry path can single it
	// out as the only class worth another attempt through a different NVIDIA key
	// or proxy exit. A 404/401/403 answer describes the model or the credential
	// itself, so it deliberately stays a plain ErrManualTestRequired.
	ErrUpstreamUnreachable   = fmt.Errorf("%w: upstream did not return a usable answer", ErrManualTestRequired)
	ErrProviderMismatch      = errors.New("model provider does not match the selected test channel")
	ErrProviderNotRoutable   = errors.New("model provider is not routable")
	ErrProviderNotConfigured = errors.New("model provider is not configured")
)

const (
	CapabilityVision    = "vision"
	CapabilityReasoning = "reasoning"
	CapabilityTools     = "tools"
)

// UnsupportedCapabilityError keeps the rejected capability available to API
// error mapping while preserving errors.Is(err, ErrCapabilityUnsupported).
type UnsupportedCapabilityError struct {
	Capability string
}

func (e *UnsupportedCapabilityError) Error() string {
	if e == nil || e.Capability == "" {
		return ErrCapabilityUnsupported.Error()
	}
	return fmt.Sprintf("%s: %s", ErrCapabilityUnsupported, e.Capability)
}

func (e *UnsupportedCapabilityError) Unwrap() error {
	return ErrCapabilityUnsupported
}

func normalizeSelection(selection Selection) (Selection, error) {
	if strings.TrimSpace(selection.PublicID) == "" || strings.TrimSpace(selection.UpstreamID) == "" || strings.TrimSpace(selection.DisplayName) == "" {
		return Selection{}, errors.New("model IDs and display name are required")
	}
	selection.Provider = strings.TrimSpace(selection.Provider)
	if selection.Provider == "" {
		selection.Provider = defaultModelProvider
	}
	if err := validateEnabledProvider(selection.Provider, selection.Enabled); err != nil {
		return Selection{}, err
	}
	if !validKind(selection.Kind) {
		return Selection{}, fmt.Errorf("unsupported model kind %q", selection.Kind)
	}
	if err := normalizeReasoningStatus(&selection); err != nil {
		return Selection{}, err
	}
	if err := normalizeToolsStatus(&selection); err != nil {
		return Selection{}, err
	}
	if err := normalizeReasoningProfile(&selection); err != nil {
		return Selection{}, err
	}
	if selection.Enabled && requiresVerification(selection.Kind) && selection.CapabilityVerifiedAt == nil {
		return Selection{}, ErrCapabilityUnverified
	}
	return selection, nil
}

func normalizeToolsStatus(selection *Selection) error {
	if selection.ToolsStatus == "" {
		if selection.SupportsTools {
			selection.ToolsStatus = ToolsStatusInferred
		} else {
			selection.ToolsStatus = ToolsStatusUnknown
		}
	}
	switch selection.ToolsStatus {
	case ToolsStatusUnknown, ToolsStatusInferred, ToolsStatusSupported, ToolsStatusUnsupported:
	default:
		return fmt.Errorf("unsupported tools status %q", selection.ToolsStatus)
	}
	if selection.ToolsStatus == ToolsStatusSupported && !selection.SupportsTools {
		return errors.New("supported tools status requires tools support")
	}
	if selection.ToolsStatus == ToolsStatusInferred && !selection.SupportsTools {
		return errors.New("inferred tools status requires tools support")
	}
	if selection.ToolsStatus == ToolsStatusUnsupported && selection.SupportsTools {
		return errors.New("unsupported tools status cannot claim tools support")
	}
	return nil
}

func normalizeReasoningStatus(selection *Selection) error {
	if selection.ReasoningStatus == "" {
		if selection.SupportsReasoning {
			selection.ReasoningStatus = ReasoningStatusInferred
		} else {
			selection.ReasoningStatus = ReasoningStatusUnknown
		}
	}
	switch selection.ReasoningStatus {
	case ReasoningStatusUnknown, ReasoningStatusInferred, ReasoningStatusVisible, ReasoningStatusHidden, ReasoningStatusUnsupported:
	default:
		return fmt.Errorf("unsupported reasoning status %q", selection.ReasoningStatus)
	}
	if !selection.SupportsReasoning && (selection.ReasoningStatus == ReasoningStatusVisible || selection.ReasoningStatus == ReasoningStatusHidden) {
		return errors.New("visible or hidden reasoning status requires reasoning support")
	}
	if selection.SupportsReasoning && selection.ReasoningStatus == ReasoningStatusUnsupported {
		return errors.New("unsupported reasoning status cannot claim reasoning support")
	}
	return nil
}

func normalizeReasoningProfile(selection *Selection) error {
	if selection.ReasoningWireFormat == "" {
		if selection.SupportsReasoning {
			selection.ReasoningWireFormat = "openai"
		} else {
			selection.ReasoningWireFormat = "none"
		}
	}
	if selection.ReasoningWireFormat == "chain_of_thought" {
		selection.ReasoningWireFormat = "thinking"
	}
	switch selection.ReasoningWireFormat {
	case "none":
		if selection.SupportsReasoning {
			return errors.New("reasoning capability and wire format disagree")
		}
	case "openai", "thinking":
		if !selection.SupportsReasoning {
			return errors.New("reasoning wire format requires reasoning support")
		}
	default:
		return fmt.Errorf("unsupported reasoning wire format %q", selection.ReasoningWireFormat)
	}
	if selection.ReasoningMinBudget < 0 || selection.ReasoningMaxBudget < 0 {
		return errors.New("reasoning budgets must be non-negative")
	}
	if selection.ReasoningMaxBudget == 0 {
		selection.ReasoningMaxBudget = 128000
	}
	if selection.ReasoningMinBudget > selection.ReasoningMaxBudget {
		return errors.New("reasoning minimum budget exceeds maximum budget")
	}
	hasProfile := len(selection.ReasoningLevels) > 0 || selection.ReasoningMinBudget > 0 || selection.ReasoningMaxBudget != 128000 || selection.ReasoningZeroAllowed || selection.ReasoningDynamicAllowed
	if selection.SupportsReasoning && !hasProfile {
		selection.ReasoningLevels = defaultReasoningLevels()
		selection.ReasoningZeroAllowed = true
		selection.ReasoningDynamicAllowed = true
	}
	if !selection.SupportsReasoning && len(selection.ReasoningLevels) == 0 {
		selection.ReasoningLevels = []string{"none"}
	}
	for _, level := range selection.ReasoningLevels {
		if !validReasoningLevel(level) {
			return fmt.Errorf("unsupported reasoning level %q", level)
		}
	}
	return nil
}

func defaultReasoningLevels() []string {
	return []string{"none", "auto", "minimal", "low", "medium", "high", "xhigh", "max"}
}

func validReasoningLevel(level string) bool {
	switch level {
	case "none", "auto", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func validateRequirements(model Model, requirements Requirements) error {
	if model.Kind != requirements.Kind {
		return ErrModelKindMismatch
	}
	if requirements.Vision && !model.SupportsVision {
		return &UnsupportedCapabilityError{Capability: CapabilityVision}
	}
	// Reasoning is deliberately not gated: the reasoning aliases are forwarded
	// verbatim, so the upstream — not the local profile — decides what it can
	// act on. Requirements.Reasoning remains observability-only.
	if requirements.Tools {
		if !model.SupportsTools {
			return &UnsupportedCapabilityError{Capability: CapabilityTools}
		}
		// "inferred" is an explicit claim: an operator PATCHed supports_tools, or
		// the capability-hint registry asserted it. The probe cannot verify several
		// gateway models (they ignore tool_choice:"required"), so demanding
		// probe-verified support deadlocked them at 501 forever. Probe-refuted
		// models carry "unsupported" and stay blocked; models nobody ever claimed
		// anything about carry "unknown" and stay blocked.
		if model.ToolsStatus != ToolsStatusSupported && model.ToolsStatus != ToolsStatusInferred {
			return ErrCapabilityUnverified
		}
	}
	if requiresVerification(model.Kind) && model.CapabilityVerifiedAt == nil {
		return ErrCapabilityUnverified
	}
	return nil
}

func validKind(kind Kind) bool {
	switch kind {
	case KindChat, KindEmbedding, KindASR, KindTTS:
		return true
	default:
		return false
	}
}

func requiresVerification(kind Kind) bool {
	return kind == KindASR || kind == KindTTS
}
