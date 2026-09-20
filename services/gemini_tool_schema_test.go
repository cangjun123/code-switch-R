package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const geminiJSONSchemaRequest = `{
  "contents":[{"role":"user","parts":[{"text":"List /tmp"}]}],
  "tools":[{"functionDeclarations":[{"name":"run_command","parametersJsonSchema":{
    "type":"object",
    "properties":{"CommandLine":{"type":"string"},"Cwd":{"type":"string"},"WaitMsBeforeAsync":{"type":"integer"},"toolSummary":{"type":"string"},"toolAction":{"type":"string"}},
    "required":["CommandLine","Cwd","WaitMsBeforeAsync","toolSummary","toolAction"],
    "additionalProperties":false
  }}]}],
  "generationConfig":{"maxOutputTokens":2048},
  "toolConfig":{"functionCallingConfig":{"mode":"ANY"}}
}`

func TestAdaptGeminiToolSchemasPreservesDefinitions(t *testing.T) {
	body := []byte(`{
	  "tools":[
	    {"googleSearch":{}},
	    {"functionDeclarations":[
	      {"name":"nested","description":"Keep this description","parametersJsonSchema":{
	        "$defs":{"entry":{"type":"string","enum":["a","b"]}},
	        "type":"object","properties":{
	          "entries":{"type":"array","items":{"$ref":"#/$defs/entry"},"minItems":1},
	          "config":{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"],"additionalProperties":false}
	        },"required":["entries","config"],"additionalProperties":false
	      }},
	      {"name":"existing","parameters":{"type":"object","properties":{}}}
	    ]},
	    {"functionDeclarations":[{"name":"other","parametersJsonSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}
	  ],
	  "contents":[{"role":"user","parts":[{"text":"parametersJsonSchema is also ordinary text"}]}],
	  "generationConfig":{"temperature":0.25},
	  "toolConfig":{"functionCallingConfig":{"mode":"ANY"}}
	}`)
	original := bytes.Clone(body)
	adapted, err := adaptGeminiToolSchemas(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, original) {
		t.Fatal("modified the original request buffer; failover must receive the original schema")
	}
	for _, path := range []string{"tools.1.functionDeclarations.0", "tools.2.functionDeclarations.0"} {
		if gjson.GetBytes(adapted, path+".parametersJsonSchema").Exists() {
			t.Fatalf("original schema key remains at %s", path)
		}
		if got, want := gjson.GetBytes(adapted, path+".parameters").Raw, gjson.GetBytes(body, path+".parametersJsonSchema").Raw; got != want {
			t.Fatalf("schema contents changed at %s:\n%s\n%s", path, got, want)
		}
	}
	for _, path := range []string{"tools.0", "tools.1.functionDeclarations.0.description", "tools.1.functionDeclarations.1", "contents", "generationConfig", "toolConfig"} {
		if gjson.GetBytes(adapted, path).Raw != gjson.GetBytes(body, path).Raw {
			t.Fatalf("unrelated request content changed at %s", path)
		}
	}
	again, err := adaptGeminiToolSchemas(adapted)
	if err != nil || !bytes.Equal(again, adapted) {
		t.Fatal("schema adaptation must be idempotent", err)
	}
}

func TestAdaptGeminiToolSchemasLeavesOtherRequestsUnchanged(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"tools":[]}`, `{"tools":{"functionDeclarations":[]}}`,
		`{"contents":[{"parts":[{"text":"parametersJsonSchema"}]}]}`,
		`{"tools":[{"functionDeclarations":[{"name":"x","parameters":{"type":"object"}}]}]}`,
		`{"tools":[{"functionDeclarations":[{"name":"x","parameters":{"type":"object"},"parametersJsonSchema":{"type":"object"}}]}]}`,
		`{"tools":[{"functionDeclarations":[{"name":"x","parametersJsonSchema":null}]}]}`,
		`{"tools":[{"functionDeclarations":[{"name":"x","parametersJsonSchema":false}]}]}`,
		`{"tools":[{"functionDeclarations":[{"parametersJsonSchema":`,
	} {
		got, err := adaptGeminiToolSchemas([]byte(body))
		if err != nil || string(got) != body {
			t.Fatalf("request should pass through unchanged: %s; got=%s err=%v", body, got, err)
		}
	}
}

// Verify the real relay route adapts tool definitions only for enabled providers,
// for both generateContent and streamGenerateContent, and also joins SSE fragments.
func TestGeminiToolSchemaRelay(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%t/stream=%t", enabled, stream), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					if !enabled && string(body) != geminiJSONSchemaRequest {
						t.Error("disabled provider must receive byte-identical request")
					}
					path := "tools.0.functionDeclarations.0."
					if enabled {
						if gjson.GetBytes(body, path+"parametersJsonSchema").Exists() {
							t.Error("unsupported schema key still present")
						}
						if got, want := gjson.GetBytes(body, path+"parameters").Raw, gjson.Get(geminiJSONSchemaRequest, path+"parametersJsonSchema").Raw; got != want {
							t.Error("required fields or constraints changed")
						}
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, fragA+fragB+terminal)
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, geminiSSEEventData(completeFC))
					}
				}))
				defer upstream.Close()
				router, key := newGeminiStitchTestEnv(t, "schema-test", upstream.URL, enabled)
				endpoint := "/gemini/v1beta/models/gemini-3.8-flash:generateContent"
				if stream {
					endpoint = "/gemini/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse"
				}
				req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(geminiJSONSchemaRequest))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("x-goog-api-key", key.Key)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, req)
				if response.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if stream && enabled {
					fc := gjson.Get(geminiSSEEventData(response.Body.String()), "candidates.0.content.parts.0.functionCall")
					var args map[string]any
					if err := json.Unmarshal([]byte(fc.Get("args").Raw), &args); err != nil {
						t.Fatal(err)
					}
					if strings.Count(response.Body.String(), "data:") != 2 || fc.Get("name").String() != "run_command" || len(args) != len(fragArgs) {
						t.Fatalf("tool response was not stitched correctly: %s", response.Body.String())
					}
				} else if stream && response.Body.String() != fragA+fragB+terminal {
					t.Fatal("disabled streaming response changed")
				} else if !stream && response.Body.String() != geminiSSEEventData(completeFC) {
					t.Fatal("non-streaming response changed")
				}
			})
		}
	}
}
