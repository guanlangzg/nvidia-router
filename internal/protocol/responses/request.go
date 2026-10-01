package responses

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"nvidia-router/internal/compat"
	"nvidia-router/internal/modelcatalog"
)

const maxResponsesBytes = 32 << 20

// Request is the validated, model-independent representation of a Responses
// request. It keeps only normalized messages and Chat fields needed after model
// resolution; request input and tool output are not retained by the response
// conversion layer.
type Request struct {
	publicModelID string
	stream        bool
	messages      []chatMessage
	chatFields    map[string]json.RawMessage
	config        ResponseConfig
	requirements  modelcatalog.Requirements
	tools         []compat.ToolDefinition
	toolChoice    compat.ToolChoice
	toolChoiceSet bool
	reasoning     compat.ReasoningSpec
}

// Parse validates and normalizes a Responses request before model resolution.
// Model capability and public-to-upstream ID checks remain in MarshalFor because
// they require the resolved catalog entry.
func Parse(body []byte) (Request, error) {
	if len(body) > maxResponsesBytes {
		return Request{}, invalidResponses("request_too_large", "body", "The request body exceeds the 32 MiB limit.")
	}
	fields, err := decodeObject(body)
	if err != nil {
		return Request{}, err
	}
	if err := rejectUnsupportedTopLevel(fields); err != nil {
		return Request{}, err
	}
	if err := rejectHostedTools(fields); err != nil {
		return Request{}, err
	}
	modelID, err := requiredModel(fields)
	if err != nil {
		return Request{}, err
	}
	stream, err := parseStream(fields["stream"])
	if err != nil {
		return Request{}, err
	}
	messages, inputReq, err := convertInput(fields)
	if err != nil {
		return Request{}, err
	}
	if instruction, ok := fields["instructions"]; ok {
		systemMessage, err := convertInstructions(instruction)
		if err != nil {
			return Request{}, err
		}
		if systemMessage != nil {
			messages = append([]chatMessage{*systemMessage}, messages...)
		}
	}
	if len(messages) == 0 {
		return Request{}, invalidResponses("missing_required_parameter", "input", "The input parameter is required.")
	}
	chat := map[string]json.RawMessage{}
	messagesBytes, _ := json.Marshal(messages)
	chat["messages"] = messagesBytes

	toolNames, err := mapTools(fields, chat)
	if err != nil {
		return Request{}, err
	}
	if err := mapToolChoice(fields, chat, toolNames); err != nil {
		return Request{}, err
	}
	if err := mapParallelToolCalls(fields, chat, len(toolNames) > 0); err != nil {
		return Request{}, err
	}
	if err := mapReasoning(fields, chat); err != nil {
		return Request{}, err
	}
	normalizedTools, err := compat.NormalizeTools(fields["tools"], compat.ToolFormatResponses, "tools")
	if err != nil {
		return Request{}, compatRequestError(err)
	}
	normalizedChoice, err := compat.NormalizeToolChoice(fields["tool_choice"], toolNames, "tool_choice")
	if err != nil {
		return Request{}, compatRequestError(err)
	}
	reasoning, err := compat.ParseReasoning(fields)
	if err != nil {
		// Pass-through: mapReasoning forwards the aliases verbatim, so a
		// malformed or conflicting pair is the upstream's verdict, not ours.
		// The parse result feeds observability and the Requirements flag only.
		reasoning = compat.ReasoningSpec{}
	}
	if err := mapMaxOutputTokens(fields, chat); err != nil {
		return Request{}, err
	}
	mapSamplingParameters(fields, chat)
	if err := mapStreamOptions(fields, chat, stream); err != nil {
		return Request{}, err
	}
	if err := mapTextFormat(fields, chat); err != nil {
		return Request{}, err
	}
	streamRaw, _ := json.Marshal(stream)
	chat["stream"] = streamRaw
	config, err := responseConfigFromFields(fields, toolNames)
	if err != nil {
		return Request{}, err
	}
	delete(chat, "messages")
	return Request{
		publicModelID: modelID,
		stream:        stream,
		messages:      messages,
		chatFields:    chat,
		config:        config,
		requirements: modelcatalog.Requirements{
			Kind:      modelcatalog.KindChat,
			Vision:    inputReq.vision,
			Tools:     inputReq.tools || len(normalizedTools) > 0 || normalizedChoice.Mode == "function" || normalizedChoice.Mode == "required",
			Reasoning: reasoning.RequiresReasoning(),
		},
		tools: normalizedTools, toolChoice: normalizedChoice,
		toolChoiceSet: len(fields["tool_choice"]) > 0 && !isJSONNull(fields["tool_choice"]), reasoning: reasoning,
	}, nil
}

func (r Request) PublicModelID() string { return r.publicModelID }

func (r Request) Stream() bool { return r.stream }

func (r Request) Requirements() modelcatalog.Requirements { return r.requirements }

func (r Request) ResponseConfig() ResponseConfig { return r.config }

// RequestedReasoningLevel returns the normalized reasoning level the caller
// asked for, or "" when reasoning was not requested. It is the zero-cost
// equivalent of re-parsing the original payload with ReasoningLevelFromBody:
// Parse already ran compat.ParseReasoning on the request fields.
func (r Request) RequestedReasoningLevel() string {
	if r.reasoning.Requested && r.reasoning.Level != "" {
		return string(r.reasoning.Level)
	}
	return ""
}

func (r Request) ReasoningRequested() bool {
	return r.reasoning.Requested
}

func (r Request) MarshalFor(model modelcatalog.Model) ([]byte, error) {
	return r.MarshalForWithOptions(model)
}

// MarshalForWithOptions renders the upstream chat payload. The Responses
// reasoning parameter is mapped to its chat field name (mapReasoning) and
// otherwise forwarded verbatim — the per-model reasoning profile is advisory
// metadata, never a rewrite authority.
func (r Request) MarshalForWithOptions(model modelcatalog.Model) ([]byte, error) {
	if model.Kind != modelcatalog.KindChat || !model.Enabled {
		return nil, invalidResponses("model_capability_unsupported", "model", "The selected model is not a chat model.")
	}
	if r.publicModelID != model.PublicID {
		return nil, invalidResponses("invalid_parameter", "model", "The model parameter does not match the resolved model.")
	}
	chat := make(map[string]json.RawMessage, len(r.chatFields)+2)
	for name, raw := range r.chatFields {
		chat[name] = cloneRaw(raw)
	}
	if len(r.tools) > 0 {
		encodedTools, err := compat.MarshalTools(r.tools, compat.ToolFormatChat)
		if err != nil {
			return nil, fmt.Errorf("marshal normalized Responses tools: %w", err)
		}
		chat["tools"] = encodedTools
	}
	if r.toolChoiceSet {
		encodedChoice, err := compat.MarshalToolChoice(r.toolChoice)
		if err != nil {
			return nil, fmt.Errorf("marshal normalized Responses tool choice: %w", err)
		}
		chat["tool_choice"] = encodedChoice
	}
	encodedModel, _ := json.Marshal(model.UpstreamID)
	chat["model"] = encodedModel
	messagesBytes, _ := json.Marshal(r.messages)
	chat["messages"] = messagesBytes
	return marshalStable(chat)
}

// ToChat keeps the original package-level conversion API for existing callers.
func ToChat(body []byte, model modelcatalog.Model) ([]byte, error) {
	request, err := Parse(body)
	if err != nil {
		return nil, err
	}
	return request.MarshalFor(model)
}

type inputRequirements struct {
	vision bool
	tools  bool
}

// convertInstructions turns the Responses instructions string into a preceding
// system message, matching the Chat protocol's system role.
func convertInstructions(raw json.RawMessage) (*chatMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return nil, invalidResponses("invalid_parameter", "instructions", "The instructions parameter must be a string.")
	}
	if value == "" {
		return nil, nil
	}
	return &chatMessage{Role: "system", RolePresent: true, Content: value, ContentPresent: true}, nil
}

func convertInput(fields map[string]json.RawMessage) ([]chatMessage, inputRequirements, error) {
	raw, ok := fields["input"]
	if !ok || string(raw) == "null" {
		return nil, inputRequirements{}, nil
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		if asString == "" {
			return nil, inputRequirements{}, invalidResponses("invalid_parameter", "input", "The input string must be non-empty.")
		}
		return []chatMessage{{Role: "user", RolePresent: true, Content: asString, ContentPresent: true}}, inputRequirements{}, nil
	}

	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil || item == nil {
			return nil, inputRequirements{}, invalidResponses("invalid_parameter", "input", "The input parameter must be a string, object, or array.")
		}
		items = []json.RawMessage{raw}
	}

	messages := make([]chatMessage, 0, len(items))
	requirements := inputRequirements{}
	for index, item := range items {
		message, wantsTools, wantsVision, err := convertInputItem(item, index)
		if err != nil {
			return nil, inputRequirements{}, err
		}
		if message.Role != "" {
			messages = append(messages, message)
		}
		requirements.tools = requirements.tools || wantsTools
		requirements.vision = requirements.vision || wantsVision
	}
	if len(messages) == 0 {
		return nil, inputRequirements{}, invalidResponses("invalid_parameter", "input", "The input array must not be empty.")
	}
	return messages, requirements, nil
}

func convertInputItem(item json.RawMessage, index int) (chatMessage, bool, bool, error) {
	var parsed responsesInputItem
	if err := json.Unmarshal(item, &parsed); err != nil {
		return chatMessage{}, false, false, invalidResponses("invalid_parameter", fmt.Sprintf("input[%d]", index), "Each input entry must be an object.")
	}
	switch parsed.Type {
	case "":
		return convertMessageItem(parsed, index)
	case "message":
		return convertMessageItem(parsed, index)
	case "function_call":
		return convertFunctionCallItem(parsed, index)
	case "function_call_output":
		return convertFunctionCallOutputItem(parsed, index)
	case "reasoning":
		// NVIDIA Chat cannot consume OpenAI reasoning state. The item is a
		// deliberate no-op so router output can be fed into the next request.
		return chatMessage{}, false, false, nil
	case "file":
		return chatMessage{}, false, false, unsupportedResponses(fmt.Sprintf("input[%d]", index), "File inputs are not supported.")
	default:
		return chatMessage{}, false, false, unsupportedResponses(fmt.Sprintf("input[%d]", index), "This input type is not supported.")
	}
}

func convertMessageItem(parsed responsesInputItem, index int) (chatMessage, bool, bool, error) {
	param := fmt.Sprintf("input[%d]", index)
	if parsed.Role == "" {
		parsed.Role = "user"
	}
	switch parsed.Role {
	case "developer", "system", "user", "assistant":
	default:
		return chatMessage{}, false, false, invalidResponses("invalid_parameter", param+".role", "The message role is not supported.")
	}
	switch parsed.Role {
	case "developer", "system", "user":
		text, usesVision, _, err := extractMessageText(parsed, param)
		if err != nil {
			return chatMessage{}, false, false, err
		}
		// Reject empty content whatever its shape: a string "", a null, an empty
		// array or an array of empty parts all produce no text, and the chat
		// path refuses empty user/system content (checklist: align semantics).
		if text == "" {
			return chatMessage{}, false, false, invalidResponses("invalid_parameter", param+".content", "The message content must be non-empty.")
		}
		return chatMessage{Role: parsed.Role, RolePresent: true, Content: text, ContentPresent: true}, false, usesVision, nil
	default: // assistant
		text, _, isString, err := extractMessageText(parsed, param)
		if err != nil {
			return chatMessage{}, false, false, err
		}
		message := chatMessage{Role: "assistant", RolePresent: true}
		if text != "" || isString {
			message.Content, message.ContentPresent = text, true
		}
		return message, false, false, nil
	}
}

func extractMessageText(parsed responsesInputItem, param string) (text string, usesVision, isString bool, err error) {
	if len(parsed.Content) == 0 {
		return "", false, true, nil
	}
	var raw string
	if json.Unmarshal(parsed.Content, &raw) == nil {
		return raw, false, true, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(parsed.Content, &parts) != nil {
		return "", false, false, invalidResponses("invalid_parameter", param+".content", "The message content must be text or an array of text parts.")
	}
	for partIndex, part := range parts {
		var kind string
		if rawType, ok := part["type"]; ok {
			_ = json.Unmarshal(rawType, &kind)
		}
		switch kind {
		case "input_text", "text", "output_text", "":
			if rawText, ok := part["text"]; ok {
				var textValue string
				_ = json.Unmarshal(rawText, &textValue)
				text += textValue
			}
		case "input_image", "image", "image_url", "output_image":
			return "", true, false, unsupportedResponses(fmt.Sprintf("%s.content[%d]", param, partIndex), "Image inputs are not supported.")
		case "file":
			return "", true, false, unsupportedResponses(fmt.Sprintf("%s.content[%d]", param, partIndex), "File inputs are not supported.")
		default:
			return "", true, false, unsupportedResponses(fmt.Sprintf("%s.content[%d]", param, partIndex), "This content type is not supported.")
		}
	}
	return text, false, false, nil
}

func convertFunctionCallItem(parsed responsesInputItem, index int) (chatMessage, bool, bool, error) {
	param := fmt.Sprintf("input[%d]", index)
	if parsed.CallID == "" || parsed.Name == "" {
		return chatMessage{}, false, false, invalidResponses("invalid_parameter", param, "Function call entries require a name and call_id.")
	}
	arguments, err := compat.NormalizeArguments(parsed.Arguments, param+".arguments")
	if err != nil {
		return chatMessage{}, false, false, compatRequestError(err)
	}
	call := chatToolCall{Index: 0, ID: parsed.CallID, Type: "function", Function: chatFunction{Name: parsed.Name, Arguments: arguments}}
	return chatMessage{Role: "assistant", RolePresent: true, ToolCalls: []chatToolCall{call}}, true, false, nil
}

func convertFunctionCallOutputItem(parsed responsesInputItem, index int) (chatMessage, bool, bool, error) {
	param := fmt.Sprintf("input[%d]", index)
	if parsed.CallID == "" {
		return chatMessage{}, false, false, invalidResponses("missing_required_parameter", param+".call_id", "Function call output entries require a call_id.")
	}
	output, err := normaliseFunctionOutput(parsed.Output, param+".output")
	if err != nil {
		return chatMessage{}, false, false, err
	}
	return chatMessage{Role: "tool", RolePresent: true, ToolCallID: parsed.CallID, ToolCallIDPresent: true, Content: output, ContentPresent: true}, true, false, nil
}

func normaliseFunctionOutput(raw json.RawMessage, param string) (string, error) {
	if len(raw) == 0 {
		return "", invalidResponses("missing_required_parameter", param, "Function call output entries require output.")
	}
	text, err := compat.FlattenToolOutput(raw, param)
	if err != nil {
		return "", compatRequestError(err)
	}
	return text, nil
}

// mapTools forwards the function tool array re-encoded as Chat function tools.
// Only function-type tools reach this stage; hosting was rejected earlier.
func mapTools(fields map[string]json.RawMessage, chat map[string]json.RawMessage) (map[string]struct{}, error) {
	raw, ok := fields["tools"]
	if !ok || isJSONNull(raw) {
		return nil, nil
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, invalidResponses("invalid_parameter", "tools", "The tools parameter must be an array.")
	}
	normalised := make([]map[string]json.RawMessage, 0, len(tools))
	names := make(map[string]struct{}, len(tools))
	for index, tool := range tools {
		var toolType string
		rawType, hasType := tool["type"]
		if !hasType || json.Unmarshal(rawType, &toolType) != nil || toolType != "function" {
			if !hasType || json.Unmarshal(rawType, &toolType) != nil {
				return nil, invalidResponses("invalid_parameter", fmt.Sprintf("tools[%d].type", index), "A function tool requires type=function.")
			}
			return nil, unsupportedResponses(fmt.Sprintf("tools[%d].type", index), "Only function tools are supported.")
		}
		definition, err := normaliseFunctionDefinition(tool, index)
		if err != nil {
			return nil, err
		}
		name := stringValue(definition["name"])
		names[name] = struct{}{}
		encoded, err := json.Marshal(definition)
		if err != nil {
			return nil, fmt.Errorf("marshal function definition: %w", err)
		}
		entry := map[string]json.RawMessage{"type": jsonRawString("function"), "function": encoded}
		normalised = append(normalised, entry)
	}
	bytes, err := json.Marshal(normalised)
	if err != nil {
		return nil, fmt.Errorf("marshal tools: %w", err)
	}
	chat["tools"] = bytes
	return names, nil
}

func normaliseResponseTools(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil || tools == nil {
		return nil, invalidResponses("invalid_parameter", "tools", "The tools parameter must be an array.")
	}
	standard := make([]map[string]json.RawMessage, 0, len(tools))
	for index, tool := range tools {
		rawType, ok := tool["type"]
		var toolType string
		if !ok || json.Unmarshal(rawType, &toolType) != nil || toolType != "function" {
			if !ok || json.Unmarshal(rawType, &toolType) != nil {
				return nil, invalidResponses("invalid_parameter", fmt.Sprintf("tools[%d].type", index), "A function tool requires type=function.")
			}
			return nil, unsupportedResponses(fmt.Sprintf("tools[%d].type", index), "Only function tools are supported.")
		}
		definition, err := normaliseFunctionDefinition(tool, index)
		if err != nil {
			return nil, err
		}
		entry := map[string]json.RawMessage{"type": jsonRawString("function")}
		for name, value := range definition {
			entry[name] = cloneRaw(value)
		}
		standard = append(standard, entry)
	}
	encoded, err := json.Marshal(standard)
	if err != nil {
		return nil, fmt.Errorf("marshal Responses tools: %w", err)
	}
	return encoded, nil
}

func normaliseResponseToolChoice(raw json.RawMessage, toolNames map[string]struct{}) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var choiceString string
	if json.Unmarshal(raw, &choiceString) == nil {
		return cloneRaw(raw), nil
	}
	var choice map[string]json.RawMessage
	if err := json.Unmarshal(raw, &choice); err != nil || choice == nil {
		return nil, invalidResponses("invalid_parameter", "tool_choice", "The tool_choice parameter is malformed.")
	}
	name := stringValue(choice["name"])
	if name == "" {
		var nested struct {
			Name string `json:"name"`
		}
		if rawFunction, exists := choice["function"]; !exists || json.Unmarshal(rawFunction, &nested) != nil {
			return nil, invalidResponses("missing_required_parameter", "tool_choice.name", "A function tool choice requires a name.")
		}
		name = nested.Name
	}
	if _, ok := toolNames[name]; !ok {
		return nil, invalidResponses("invalid_parameter", "tool_choice.name", "The selected function is not present in tools.")
	}
	encoded, err := json.Marshal(map[string]any{"type": "function", "name": name})
	if err != nil {
		return nil, fmt.Errorf("marshal Responses tool choice: %w", err)
	}
	return encoded, nil
}

func normaliseFunctionDefinition(tool map[string]json.RawMessage, index int) (map[string]json.RawMessage, error) {
	nested, hasNested := tool["function"]
	flat := make(map[string]json.RawMessage)
	for _, name := range []string{"name", "description", "parameters", "strict", "allowed_callers", "defer_loading", "output_schema"} {
		if value, ok := tool[name]; ok {
			flat[name] = value
		}
	}
	if hasNested && len(flat) > 0 {
		return nil, invalidResponses("invalid_parameter", fmt.Sprintf("tools[%d]", index), "A function tool cannot mix flat and nested function fields.")
	}
	definition := flat
	if hasNested {
		if json.Unmarshal(nested, &definition) != nil || definition == nil {
			return nil, invalidResponses("invalid_parameter", fmt.Sprintf("tools[%d].function", index), "The function definition must be an object.")
		}
	}
	allowed := map[string]struct{}{
		"name": {}, "description": {}, "parameters": {}, "strict": {},
		"allowed_callers": {}, "defer_loading": {}, "output_schema": {},
	}
	for name := range definition {
		if _, ok := allowed[name]; !ok {
			return nil, invalidResponses("invalid_parameter", fmt.Sprintf("tools[%d].%s", index, name), "Unknown function tool field.")
		}
	}
	for name := range tool {
		if name == "type" || name == "function" {
			continue
		}
		if _, ok := definition[name]; !ok {
			return nil, invalidResponses("invalid_parameter", fmt.Sprintf("tools[%d].%s", index, name), "Unknown function tool field.")
		}
	}
	if raw, ok := definition["allowed_callers"]; ok && string(raw) != "null" {
		return nil, unsupportedResponses(fmt.Sprintf("tools[%d].allowed_callers", index), "Allowed callers are not supported.")
	}
	if raw, ok := definition["defer_loading"]; ok && string(raw) != "null" {
		return nil, unsupportedResponses(fmt.Sprintf("tools[%d].defer_loading", index), "Deferred tool loading is not supported.")
	}
	if raw, ok := definition["output_schema"]; ok && string(raw) != "null" {
		return nil, unsupportedResponses(fmt.Sprintf("tools[%d].output_schema", index), "Output schemas are not supported.")
	}
	name, ok := definition["name"]
	var functionName string
	if !ok || json.Unmarshal(name, &functionName) != nil || strings.TrimSpace(functionName) != functionName || functionName == "" {
		return nil, invalidResponses("missing_required_parameter", fmt.Sprintf("tools[%d].name", index), "A function tool requires a non-empty name.")
	}
	if raw, ok := definition["description"]; ok && string(raw) != "null" {
		var description string
		if json.Unmarshal(raw, &description) != nil {
			return nil, invalidResponses("invalid_parameter", fmt.Sprintf("tools[%d].description", index), "The function description must be a string or null.")
		}
	}
	if raw, ok := definition["parameters"]; ok && string(raw) != "null" {
		var parameters map[string]json.RawMessage
		if json.Unmarshal(raw, &parameters) != nil || parameters == nil {
			return nil, invalidResponses("invalid_parameter", fmt.Sprintf("tools[%d].parameters", index), "The function parameters must be an object or null.")
		}
	}
	if raw, ok := definition["strict"]; ok && string(raw) != "null" {
		var strict bool
		if json.Unmarshal(raw, &strict) != nil {
			return nil, invalidResponses("invalid_parameter", fmt.Sprintf("tools[%d].strict", index), "The function strict flag must be a boolean or null.")
		}
	}
	for _, optional := range []string{"description", "parameters", "strict", "allowed_callers", "defer_loading", "output_schema"} {
		if raw, ok := definition[optional]; ok && string(raw) == "null" {
			delete(definition, optional)
		}
	}
	return definition, nil
}

func stringValue(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func mapToolChoice(fields map[string]json.RawMessage, chat map[string]json.RawMessage, toolNames map[string]struct{}) error {
	raw, ok := fields["tool_choice"]
	if !ok || string(raw) == "null" {
		return nil
	}
	var choiceString string
	if json.Unmarshal(raw, &choiceString) == nil {
		if choiceString != "auto" && choiceString != "none" && choiceString != "required" {
			return invalidResponses("invalid_parameter", "tool_choice", "The tool_choice string is not supported.")
		}
		if choiceString == "required" && len(toolNames) == 0 {
			return invalidResponses("invalid_parameter", "tool_choice", "required tool_choice needs at least one function tool.")
		}
		if len(toolNames) == 0 && choiceString == "auto" {
			return nil
		}
		chat["tool_choice"] = raw
		return nil
	}
	var choice map[string]json.RawMessage
	if json.Unmarshal(raw, &choice) != nil || choice == nil {
		return invalidResponses("invalid_parameter", "tool_choice", "The tool_choice parameter is malformed.")
	}
	var choiceType string
	if json.Unmarshal(choice["type"], &choiceType) != nil || choiceType != "function" {
		return unsupportedResponses("tool_choice.type", "Only function tool choices are supported.")
	}
	name := stringValue(choice["name"])
	if name == "" {
		var nested struct {
			Name string `json:"name"`
		}
		if rawFunction, exists := choice["function"]; !exists || json.Unmarshal(rawFunction, &nested) != nil {
			return invalidResponses("missing_required_parameter", "tool_choice.name", "A function tool choice requires a name.")
		}
		name = nested.Name
	}
	if _, ok := toolNames[name]; !ok {
		return invalidResponses("invalid_parameter", "tool_choice.name", "The selected function is not present in tools.")
	}
	converted, err := json.Marshal(map[string]any{"type": "function", "function": map[string]string{"name": name}})
	if err != nil {
		return fmt.Errorf("marshal tool choice: %w", err)
	}
	chat["tool_choice"] = converted
	return nil
}

func mapParallelToolCalls(fields map[string]json.RawMessage, chat map[string]json.RawMessage, hasTools bool) error {
	raw, ok := fields["parallel_tool_calls"]
	if !ok || string(raw) == "null" {
		if hasTools {
			chat["parallel_tool_calls"] = json.RawMessage("true")
		}
		return nil
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return invalidResponses("invalid_parameter", "parallel_tool_calls", "parallel_tool_calls must be a boolean or null.")
	}
	if hasTools {
		chat["parallel_tool_calls"] = raw
	}
	return nil
}

func mapReasoning(fields map[string]json.RawMessage, chat map[string]json.RawMessage) error {
	if native, ok := fields["reasoning_effort"]; ok && !isJSONNull(native) {
		chat["reasoning_effort"] = native
	}
	if raw, ok := fields["reasoning"]; ok && !isJSONNull(raw) {
		var reasoning struct {
			Effort json.RawMessage `json:"effort"`
		}
		if json.Unmarshal(raw, &reasoning) == nil && len(reasoning.Effort) > 0 {
			chat["reasoning_effort"] = reasoning.Effort
		} else {
			var value string
			if json.Unmarshal(raw, &value) == nil {
				chat["reasoning_effort"] = raw
			} else {
				chat["reasoning"] = raw
			}
		}
	}
	if raw, ok := fields["thinking"]; ok && !isJSONNull(raw) {
		chat["thinking"] = raw
	}
	return nil
}

func mapMaxOutputTokens(fields map[string]json.RawMessage, chat map[string]json.RawMessage) error {
	raw, ok := fields["max_output_tokens"]
	if !ok || isJSONNull(raw) {
		return nil
	}
	var value *int
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return invalidResponses("invalid_parameter", "max_output_tokens", "The max_output_tokens parameter must be an integer.")
	}
	if *value <= 0 {
		return invalidResponses("invalid_parameter", "max_output_tokens", "The max_output_tokens parameter must be positive.")
	}
	encoded, _ := json.Marshal(*value)
	chat["max_tokens"] = encoded
	return nil
}

// mapSamplingParameters forwards sampling controls verbatim so responses
// requests get the same generation behaviour a chat request would; dropping
// them silently (e.g. temperature: 0 becoming the default) changes output
// without the client knowing.
func mapSamplingParameters(fields map[string]json.RawMessage, chat map[string]json.RawMessage) {
	for _, name := range []string{"temperature", "top_p", "seed", "stop", "presence_penalty", "frequency_penalty", "user"} {
		if raw, ok := fields[name]; ok && !isJSONNull(raw) {
			chat[name] = raw
		}
	}
}

func mapStreamOptions(fields map[string]json.RawMessage, chat map[string]json.RawMessage, stream bool) error {
	options := map[string]json.RawMessage{}
	if raw, ok := fields["stream_options"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &options); err != nil || options == nil {
			return invalidResponses("invalid_parameter", "stream_options", "The stream_options parameter must be an object or null.")
		}
		for name, value := range options {
			switch name {
			case "include_usage":
				var includeUsage bool
				if json.Unmarshal(value, &includeUsage) != nil {
					return invalidResponses("invalid_parameter", "stream_options.include_usage", "include_usage must be a boolean.")
				}
			case "include_obfuscation":
				var includeObfuscation bool
				if json.Unmarshal(value, &includeObfuscation) != nil {
					return invalidResponses("invalid_parameter", "stream_options.include_obfuscation", "include_obfuscation must be a boolean.")
				}
				if includeObfuscation {
					return unsupportedResponses("stream_options.include_obfuscation", "Stream obfuscation is not supported.")
				}
				delete(options, name)
			default:
				return invalidResponses("invalid_parameter", "stream_options."+name, "Unknown stream option.")
			}
		}
	}
	if !stream {
		return nil
	}
	options["include_usage"] = json.RawMessage("true")
	encoded, err := json.Marshal(options)
	if err != nil {
		return fmt.Errorf("marshal stream options: %w", err)
	}
	chat["stream_options"] = encoded
	return nil
}

// mapTextFormat maps the Responses text.format (structured output) parameter to
// the Chat response_format parameter. Without this a client requesting
// json_schema output gets plain text back with no error, and its parser fails
// downstream. json_object and json_schema map directly; text (the default) is a
// no-op; anything else is rejected instead of being silently dropped.
func mapTextFormat(fields map[string]json.RawMessage, chat map[string]json.RawMessage) error {
	raw, ok := fields["text"]
	if !ok || isJSONNull(raw) {
		return nil
	}
	var text map[string]json.RawMessage
	if json.Unmarshal(raw, &text) != nil || text == nil {
		var asString string
		if json.Unmarshal(raw, &asString) == nil {
			return nil
		}
		return invalidResponses("invalid_parameter", "text", "The text parameter must be an object with a format.")
	}
	if rawVerbosity, ok := text["verbosity"]; ok && !isJSONNull(rawVerbosity) {
		var verbosity string
		if json.Unmarshal(rawVerbosity, &verbosity) != nil || (verbosity != "low" && verbosity != "medium" && verbosity != "high") {
			return invalidResponses("invalid_parameter", "text.verbosity", "text.verbosity must be low, medium, high, or null.")
		}
	}
	formatRaw, ok := text["format"]
	if !ok || isJSONNull(formatRaw) {
		return nil
	}
	var format struct {
		Type       string          `json:"type"`
		Name       string          `json:"name"`
		Schema     json.RawMessage `json:"schema"`
		Strict     *bool           `json:"strict"`
		JSONSchema json.RawMessage `json:"json_schema"`
	}
	if err := json.Unmarshal(formatRaw, &format); err != nil {
		return invalidResponses("invalid_parameter", "text.format", "The text format parameter is malformed.")
	}
	switch format.Type {
	case "", "text":
		return nil
	case "json_object":
		chat["response_format"] = json.RawMessage(`{"type":"json_object"}`)
		return nil
	case "json_schema":
		schema := format.Schema
		if len(schema) == 0 {
			schema = format.JSONSchema
		}
		if len(schema) == 0 {
			return invalidResponses("invalid_parameter", "text.format", "A json_schema format requires a schema.")
		}
		entry := map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": format.Name, "schema": json.RawMessage(schema)},
		}
		if format.Strict != nil {
			entry["json_schema"].(map[string]any)["strict"] = *format.Strict
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("marshal response_format: %w", err)
		}
		chat["response_format"] = encoded
		return nil
	default:
		return unsupportedResponses("text.format", "This text format is not supported.")
	}
}

func requiredModel(fields map[string]json.RawMessage) (string, error) {
	raw, ok := fields["model"]
	if !ok {
		return "", invalidResponses("missing_required_parameter", "model", "The model parameter is required.")
	}
	var modelID string
	if json.Unmarshal(raw, &modelID) != nil || strings.TrimSpace(modelID) != modelID || modelID == "" {
		return "", invalidResponses("invalid_parameter", "model", "The model parameter must be a non-empty string.")
	}
	return modelID, nil
}

func parseStream(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return false, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, invalidResponses("invalid_parameter", "stream", "The stream parameter must be a boolean.")
	}
	return value, nil
}

func decodeObject(body []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, invalidResponses("invalid_json", "", "The request body must be a JSON object.")
	}
	return fields, nil
}

func marshalStable(fields map[string]json.RawMessage) ([]byte, error) {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	builder.WriteByte('{')
	for index, name := range names {
		if index > 0 {
			builder.WriteByte(',')
		}
		encodedName, _ := json.Marshal(name)
		builder.Write(encodedName)
		builder.WriteByte(':')
		builder.Write(fields[name])
	}
	builder.WriteByte('}')
	if !json.Valid([]byte(builder.String())) {
		return nil, fmt.Errorf("marshal chat request: invalid raw JSON")
	}
	return []byte(builder.String()), nil
}

func jsonRawString(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
