package services

import (
	"bytes"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// adaptGeminiToolSchemas bridges gateways that accept functionDeclarations.parameters
// but silently ignore parametersJsonSchema. Move the entire schema without dropping
// required fields, nested schemas, or constraints. This is only enabled for providers
// using tool-call compatibility mode; native Gemini requests remain untouched.
func adaptGeminiToolSchemas(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"parametersJsonSchema"`)) || !gjson.ValidBytes(body) {
		return body, nil
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body, nil
	}

	result := body
	for toolIndex, tool := range tools.Array() {
		declarations := tool.Get("functionDeclarations")
		if !declarations.IsArray() {
			continue
		}
		for declarationIndex, declaration := range declarations.Array() {
			schema := declaration.Get("parametersJsonSchema")
			// Do not choose between conflicting definitions or reinterpret non-object schemas.
			if !schema.IsObject() || declaration.Get("parameters").Exists() {
				continue
			}
			path := fmt.Sprintf("tools.%d.functionDeclarations.%d", toolIndex, declarationIndex)
			var err error
			result, err = sjson.SetRawBytes(result, path+".parameters", []byte(schema.Raw))
			if err != nil {
				return body, fmt.Errorf("adapt Gemini tool schema: %w", err)
			}
			result, err = sjson.DeleteBytes(result, path+".parametersJsonSchema")
			if err != nil {
				return body, fmt.Errorf("remove adapted Gemini tool schema: %w", err)
			}
		}
	}
	return result, nil
}
