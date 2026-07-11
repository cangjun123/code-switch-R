package services

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	codexClientMultiAgentNamespace   = "collaboration"
	codexUpstreamMultiAgentNamespace = "agents"
)

// ErrCodexMultiAgentNamespaceConflict means a request defines both namespace
// names. Rewriting it would merge two independent tool namespaces.
var ErrCodexMultiAgentNamespaceConflict = errors.New("codex multi-agent namespace conflict")

type CodexProviderHistorySanitizeStats struct {
	RemovedItems        int
	RemovedContentParts int
	RemovedItemIDs      int
	RemovedTopLevelRefs int
}

func (stats CodexProviderHistorySanitizeStats) Total() int {
	return stats.RemovedItems + stats.RemovedContentParts + stats.RemovedItemIDs + stats.RemovedTopLevelRefs
}

type codexProviderHistorySanitizePlan struct {
	removedItemIDs      map[string]struct{}
	removedItemHashes   map[[sha256.Size]byte]struct{}
	removedContentParts map[[sha256.Size]byte]struct{}
	removedOutputParts  map[[sha256.Size]byte]struct{}
	strippedItemIDs     map[string]struct{}
	strippedEncrypted   map[[sha256.Size]byte]struct{}
	topLevelRefs        map[string][sha256.Size]byte
}

// SanitizeCodexProviderBoundHistory removes opaque state that cannot be
// replayed through a different Responses provider. Conversation text and tool
// call/result pairs remain intact.
func SanitizeCodexProviderBoundHistory(body []byte) ([]byte, CodexProviderHistorySanitizeStats, error) {
	plan, err := buildCodexProviderHistorySanitizePlan(body)
	if err != nil {
		return body, CodexProviderHistorySanitizeStats{}, err
	}
	return sanitizeCodexProviderBoundHistoryWithPlan(body, plan)
}

func buildCodexProviderHistorySanitizePlan(body []byte) (*codexProviderHistorySanitizePlan, error) {
	root, err := decodeJSONPreservingNumbers(body)
	if err != nil {
		return nil, err
	}
	object, ok := root.(map[string]any)
	if !ok {
		return &codexProviderHistorySanitizePlan{}, nil
	}

	plan := &codexProviderHistorySanitizePlan{
		removedItemIDs:      make(map[string]struct{}),
		removedItemHashes:   make(map[[sha256.Size]byte]struct{}),
		removedContentParts: make(map[[sha256.Size]byte]struct{}),
		removedOutputParts:  make(map[[sha256.Size]byte]struct{}),
		strippedItemIDs:     make(map[string]struct{}),
		strippedEncrypted:   make(map[[sha256.Size]byte]struct{}),
		topLevelRefs:        make(map[string][sha256.Size]byte),
	}
	for _, key := range []string{"previous_response_id", "conversation"} {
		if value, exists := object[key]; exists {
			plan.topLevelRefs[key] = hashCodexHistoryValue(value)
		}
	}

	input, ok := object["input"].([]any)
	if ok {
		for _, rawItem := range input {
			item, isObject := rawItem.(map[string]any)
			if !isObject {
				continue
			}

			itemType := stringField(item, "type")
			_, hasEncryptedContent := item["encrypted_content"]
			if itemType == "reasoning" || itemType == "item_reference" || (hasEncryptedContent && !isCodexToolOutputType(itemType)) {
				if id := stringField(item, "id"); id != "" {
					plan.removedItemIDs[id] = struct{}{}
				}
				plan.removedItemHashes[hashCodexHistoryValue(item)] = struct{}{}
				continue
			}
			if hasEncryptedContent {
				plan.strippedEncrypted[hashCodexHistoryValue(item)] = struct{}{}
			}

			if id := stringField(item, "id"); id != "" {
				plan.strippedItemIDs[id] = struct{}{}
			}
			if content, isArray := item["content"].([]any); isArray {
				for _, rawPart := range content {
					part, isPartObject := rawPart.(map[string]any)
					if isPartObject {
						_, partHasEncryptedContent := part["encrypted_content"]
						if stringField(part, "type") == "encrypted_content" || partHasEncryptedContent {
							plan.removedContentParts[hashCodexHistoryValue(part)] = struct{}{}
						}
					}
				}
			}
			if output, isArray := item["output"].([]any); isArray {
				for _, rawPart := range output {
					if isCodexEncryptedHistoryValue(rawPart) {
						plan.removedOutputParts[hashCodexHistoryValue(rawPart)] = struct{}{}
					}
				}
			}
		}
	}
	return plan, nil
}

func sanitizeCodexProviderBoundHistoryWithPlan(body []byte, plan *codexProviderHistorySanitizePlan) ([]byte, CodexProviderHistorySanitizeStats, error) {
	if plan == nil {
		return body, CodexProviderHistorySanitizeStats{}, nil
	}
	root, err := decodeJSONPreservingNumbers(body)
	if err != nil {
		return body, CodexProviderHistorySanitizeStats{}, err
	}
	object, ok := root.(map[string]any)
	if !ok {
		return body, CodexProviderHistorySanitizeStats{}, nil
	}

	stats := CodexProviderHistorySanitizeStats{}
	for key, expectedHash := range plan.topLevelRefs {
		if value, exists := object[key]; exists && hashCodexHistoryValue(value) == expectedHash {
			delete(object, key)
			stats.RemovedTopLevelRefs++
		}
	}

	if input, isArray := object["input"].([]any); isArray {
		sanitized := make([]any, 0, len(input))
		for _, rawItem := range input {
			item, isObject := rawItem.(map[string]any)
			if !isObject {
				sanitized = append(sanitized, rawItem)
				continue
			}

			id := stringField(item, "id")
			itemHash := hashCodexHistoryValue(item)
			_, removeByID := plan.removedItemIDs[id]
			_, removeByHash := plan.removedItemHashes[itemHash]
			if (id != "" && removeByID) || removeByHash {
				stats.RemovedItems++
				continue
			}
			if _, stripID := plan.strippedItemIDs[id]; id != "" && stripID {
				delete(item, "id")
				stats.RemovedItemIDs++
			}
			if _, stripEncrypted := plan.strippedEncrypted[itemHash]; stripEncrypted {
				delete(item, "encrypted_content")
				stats.RemovedContentParts++
			}
			if content, hasContent := item["content"].([]any); hasContent {
				filtered := make([]any, 0, len(content))
				for _, rawPart := range content {
					if _, remove := plan.removedContentParts[hashCodexHistoryValue(rawPart)]; remove {
						stats.RemovedContentParts++
						continue
					}
					filtered = append(filtered, rawPart)
				}
				if len(filtered) == 0 && len(content) > 0 {
					stats.RemovedItems++
					continue
				}
				item["content"] = filtered
			}
			if output, hasOutput := item["output"].([]any); hasOutput {
				filtered := make([]any, 0, len(output))
				for _, rawPart := range output {
					if _, remove := plan.removedOutputParts[hashCodexHistoryValue(rawPart)]; remove {
						stats.RemovedContentParts++
						continue
					}
					filtered = append(filtered, rawPart)
				}
				if len(filtered) == 0 && len(output) > 0 {
					stats.RemovedItems++
					continue
				}
				item["output"] = filtered
			}
			sanitized = append(sanitized, item)
		}
		object["input"] = sanitized
	}

	if stats.Total() == 0 {
		return body, stats, nil
	}
	rewritten, err := json.Marshal(root)
	if err != nil {
		return body, CodexProviderHistorySanitizeStats{}, fmt.Errorf("marshal sanitized codex provider history: %w", err)
	}
	return rewritten, stats, nil
}

func isCodexEncryptedHistoryValue(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	_, hasEncryptedContent := object["encrypted_content"]
	return hasEncryptedContent || stringField(object, "type") == "encrypted_content"
}

func hashCodexHistoryValue(value any) [sha256.Size]byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return sha256.Sum256([]byte(fmt.Sprintf("%T:%v", value, value)))
	}
	return sha256.Sum256(encoded)
}

func HasCodexMultiAgentNamespaceConflict(body []byte) (bool, error) {
	root, err := decodeJSONPreservingNumbers(body)
	if err != nil {
		return false, err
	}
	definitions := codexNamespaceDefinitions{}
	inspectCodexNamespaceDefinitions(root, &definitions)
	return definitions.collaboration && definitions.agents, nil
}

// RewriteCodexMultiAgentRequest rewrites only structured namespace tool
// definitions and tool-call history. Unrelated strings are never inspected.
func RewriteCodexMultiAgentRequest(body []byte) ([]byte, int, error) {
	root, err := decodeJSONPreservingNumbers(body)
	if err != nil {
		return body, 0, err
	}

	definitions := codexNamespaceDefinitions{}
	inspectCodexNamespaceDefinitions(root, &definitions)
	if definitions.collaboration && definitions.agents {
		return body, 0, ErrCodexMultiAgentNamespaceConflict
	}

	modified := rewriteCodexNamespaceDefinitions(root, codexClientMultiAgentNamespace, codexUpstreamMultiAgentNamespace)
	modified += rewriteCodexRequestCallNamespaces(root, codexClientMultiAgentNamespace, codexUpstreamMultiAgentNamespace)
	if modified == 0 {
		return body, 0, nil
	}

	rewritten, err := json.Marshal(root)
	if err != nil {
		return body, 0, fmt.Errorf("marshal rewritten codex request: %w", err)
	}
	return rewritten, modified, nil
}

// stripCodexInputNamespaces removes only direct namespace fields from replayed
// input items. Namespace tool definitions and nested tool data stay untouched.
func stripCodexInputNamespaces(body []byte) ([]byte, int, error) {
	root, err := decodeJSONPreservingNumbers(body)
	if err != nil {
		return body, 0, err
	}
	object, ok := root.(map[string]any)
	if !ok {
		return body, 0, nil
	}
	input, ok := object["input"].([]any)
	if !ok {
		return body, 0, nil
	}

	removed := 0
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := item["namespace"]; exists {
			delete(item, "namespace")
			removed++
		}
	}
	if removed == 0 {
		return body, 0, nil
	}
	rewritten, err := json.Marshal(root)
	if err != nil {
		return body, 0, fmt.Errorf("marshal codex input namespace fallback: %w", err)
	}
	return rewritten, removed, nil
}

// RewriteCodexMultiAgentResponse maps upstream tool calls back to the
// namespace understood by the client.
func RewriteCodexMultiAgentResponse(body []byte) ([]byte, int, error) {
	root, err := decodeJSONPreservingNumbers(body)
	if err != nil {
		return body, 0, err
	}

	modified := rewriteCodexResponseCallNamespaces(root, codexUpstreamMultiAgentNamespace, codexClientMultiAgentNamespace)
	if modified == 0 {
		return body, 0, nil
	}

	rewritten, err := json.Marshal(root)
	if err != nil {
		return body, 0, fmt.Errorf("marshal rewritten codex response: %w", err)
	}
	return rewritten, modified, nil
}

// NewCodexMultiAgentNamespaceSSEHook rewrites JSON payloads on data: lines.
// SSE framing, non-JSON data, and the [DONE] sentinel pass through unchanged.
func NewCodexMultiAgentNamespaceSSEHook(modified *int) func([]byte) (bool, []byte) {
	return func(line []byte) (bool, []byte) {
		const dataPrefix = "data:"
		if !bytes.HasPrefix(line, []byte(dataPrefix)) {
			return true, line
		}

		payloadWithSpacing := line[len(dataPrefix):]
		start := 0
		for start < len(payloadWithSpacing) && isSSESpacing(payloadWithSpacing[start]) {
			start++
		}
		end := len(payloadWithSpacing)
		for end > start && isSSESpacing(payloadWithSpacing[end-1]) {
			end--
		}
		payload := payloadWithSpacing[start:end]
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			return true, line
		}

		rewritten, count, err := RewriteCodexMultiAgentResponse(payload)
		if err != nil || count == 0 {
			return true, line
		}
		if modified != nil {
			*modified += count
		}

		output := make([]byte, 0, len(line)-len(payload)+len(rewritten))
		output = append(output, dataPrefix...)
		output = append(output, payloadWithSpacing[:start]...)
		output = append(output, rewritten...)
		output = append(output, payloadWithSpacing[end:]...)
		return true, output
	}
}

type codexNamespaceDefinitions struct {
	collaboration bool
	agents        bool
}

func decodeJSONPreservingNumbers(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode codex namespace payload: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	return root, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("decode codex namespace payload: multiple JSON values")
		}
		return fmt.Errorf("decode codex namespace payload trailing data: %w", err)
	}
	return nil
}

func inspectCodexNamespaceDefinitions(root any, found *codexNamespaceDefinitions) {
	visitCodexRequestToolCollections(root, func(collection any) {
		visitCodexNamespaceDefinitions(collection, func(definition map[string]any) {
			switch stringField(definition, "name") {
			case codexClientMultiAgentNamespace:
				found.collaboration = true
			case codexUpstreamMultiAgentNamespace:
				found.agents = true
			}
		})
	})
}

func rewriteCodexNamespaceDefinitions(root any, from, to string) int {
	modified := 0
	visitCodexRequestToolCollections(root, func(collection any) {
		visitCodexNamespaceDefinitions(collection, func(definition map[string]any) {
			if stringField(definition, "name") == from {
				definition["name"] = to
				modified++
			}
		})
	})
	return modified
}

func visitCodexRequestToolCollections(root any, visit func(any)) {
	object, ok := root.(map[string]any)
	if !ok {
		return
	}
	for key, collection := range object {
		if isCodexToolCollectionKey(key) {
			visit(collection)
		}
	}

	input, ok := object["input"].([]any)
	if !ok {
		return
	}
	for _, value := range input {
		item, ok := value.(map[string]any)
		if !ok || stringField(item, "type") != "additional_tools" {
			continue
		}
		if tools, ok := item["tools"]; ok {
			visit(tools)
		}
	}
}

// visitCodexNamespaceDefinitions accepts the standard array form plus the
// object/items wrapper used by some Responses-compatible relays. It does not
// descend into JSON schemas or tool parameters.
func visitCodexNamespaceDefinitions(value any, visit func(map[string]any)) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			visitCodexNamespaceDefinitionItem(item, visit)
		}
	case map[string]any:
		visitCodexNamespaceDefinitionItem(typed, visit)
		if items, ok := typed["items"]; ok {
			visitCodexNamespaceDefinitions(items, visit)
		}
	}
}

func visitCodexNamespaceDefinitionItem(value any, visit func(map[string]any)) {
	definition, ok := value.(map[string]any)
	if !ok {
		return
	}
	if stringField(definition, "type") == "namespace" {
		visit(definition)
	}
}

func rewriteCodexRequestCallNamespaces(root any, from, to string) int {
	object, ok := root.(map[string]any)
	if !ok {
		return 0
	}
	return rewriteCodexCallCollection(object["input"], from, to)
}

func rewriteCodexResponseCallNamespaces(root any, from, to string) int {
	object, ok := root.(map[string]any)
	if !ok {
		return 0
	}

	modified := rewriteCodexCallItem(object, from, to)
	modified += rewriteCodexCallCollection(object["output"], from, to)
	modified += rewriteCodexCallItem(object["item"], from, to)
	if response, ok := object["response"].(map[string]any); ok {
		modified += rewriteCodexCallCollection(response["output"], from, to)
	}
	return modified
}

func rewriteCodexCallCollection(value any, from, to string) int {
	switch typed := value.(type) {
	case []any:
		modified := 0
		for _, item := range typed {
			modified += rewriteCodexCallItem(item, from, to)
		}
		return modified
	default:
		return rewriteCodexCallItem(value, from, to)
	}
}

func rewriteCodexCallItem(value any, from, to string) int {
	item, ok := value.(map[string]any)
	if !ok || !isCodexCallType(stringField(item, "type")) || stringField(item, "namespace") != from {
		return 0
	}
	item["namespace"] = to
	return 1
}

func isCodexCallType(itemType string) bool {
	return itemType == "function_call" || itemType == "custom_tool_call"
}

func stringField(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func isCodexToolCollectionKey(key string) bool {
	return key == "tools" || key == "additional_tools"
}

func isSSESpacing(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r'
}
