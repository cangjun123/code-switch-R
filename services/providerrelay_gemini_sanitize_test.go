package services

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"
)

// assertGenaiParsable mirrors go-genai iterateResponseStream: every non-empty
// line must start with "data:" and carry a JSON object.
func assertGenaiParsable(t *testing.T, out string) {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		prefix, data, _ := bytes.Cut(line, []byte(":"))
		if string(prefix) != "data" {
			t.Fatalf("genai would reject stream chunk %q", line)
		}
		if p := strings.TrimSpace(string(data)); !strings.HasPrefix(p, "{") {
			t.Fatalf("genai would fail to unmarshal %q", line)
		}
	}
}

func TestStreamGeminiSanitizeDropsNonDataLines(t *testing.T) {
	body := ":\n\n" +
		": ping\n\n" +
		"event: message\nid: 1\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n" +
		"retry: 1000\n\n" +
		"data: {\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2}}\r\n\r\n" +
		"data: [DONE]\n\n"
	var out bytes.Buffer
	log := &ReqeustLog{}
	if err := streamGeminiResponseWithHook(strings.NewReader(body), &out, log, time.Now(), geminiStreamOptions{sanitize: true}); err != nil {
		t.Fatal(err)
	}
	assertGenaiParsable(t, out.String())
	if !strings.Contains(out.String(), `"text":"hi"`) || !strings.Contains(out.String(), "usageMetadata") {
		t.Fatalf("data events lost: %q", out.String())
	}
	if log.InputTokens != 3 || log.OutputTokens != 2 {
		t.Fatalf("usage not parsed: in=%d out=%d", log.InputTokens, log.OutputTokens)
	}
}

func TestStreamGeminiSanitizeKeepsCleanEventsVerbatim(t *testing.T) {
	body := "data: {\"a\":1}\r\n\r\ndata: {\"b\":2}\n\n"
	var out bytes.Buffer
	if err := streamGeminiResponseWithHook(strings.NewReader(body), &out, &ReqeustLog{}, time.Now(), geminiStreamOptions{sanitize: true}); err != nil {
		t.Fatal(err)
	}
	if out.String() != body {
		t.Fatalf("clean stream modified: %q", out.String())
	}
}

func TestStreamGeminiSanitizeHeartbeatBetweenFragments(t *testing.T) {
	body := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"run_command","args":{}}}]}}]}` + "\n\n" +
		": keepalive\n\n" +
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"","args":{"CommandLine":"ls"}}}]}}]}` + "\n\n"
	var out bytes.Buffer
	if err := streamGeminiResponseWithHook(strings.NewReader(body), &out, &ReqeustLog{}, time.Now(), geminiStreamOptions{stitcher: &geminiFunctionCallStitcher{}, sanitize: true}); err != nil {
		t.Fatal(err)
	}
	assertGenaiParsable(t, out.String())
	if strings.Count(out.String(), "functionCall") != 1 || !strings.Contains(out.String(), `"name":"run_command","args":{"CommandLine":"ls"}`) {
		t.Fatalf("fragments not stitched across heartbeat: %q", out.String())
	}
}

func TestStreamGeminiNoSanitizePassesThrough(t *testing.T) {
	// streamGenerateContent without alt=sse returns a JSON array; must stay untouched.
	body := "[{\"candidates\":[]}\n,{\"candidates\":[]}]"
	var out bytes.Buffer
	if err := streamGeminiResponseWithHook(strings.NewReader(body), &out, &ReqeustLog{}, time.Now(), geminiStreamOptions{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != body {
		t.Fatalf("non-SSE stream modified: %q", out.String())
	}
}
