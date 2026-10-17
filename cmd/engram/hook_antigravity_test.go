package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAntigravitySessionStartUsesNativeContract(t *testing.T) {
	cwd := t.TempDir()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/project/current":
			if r.URL.Query().Get("cwd") != cwd {
				t.Error("incorrect workspace")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"project": "payments", "project_source": "config"})
		case "/sessions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["id"] != "ag-runtime" || body["directory"] != cwd || body["ownership_mode"] != "project_owned" {
				t.Errorf("incorrect registration: %v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "ag-runtime", "status": "created"})
		case "/context":
			_ = json.NewEncoder(w).Encode(map[string]string{"context": "Deduplicate by event_id."})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer provider.Close()
	t.Setenv("ENGRAM_URL", provider.URL)
	input, _ := json.Marshal(map[string]any{"conversationId": "ag-runtime", "workspacePaths": []string{cwd}})
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write(input)
	_ = writer.Close()
	previous := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() { os.Stdin = previous; _ = reader.Close() })
	stubExitWithPanic(t)
	stdout, stderr, exit := captureOutputAndRecover(t, func() { cmdHook([]string{"antigravity-session-start"}) })
	var response struct {
		InjectSteps []struct {
			SystemMessage struct {
				SystemMessage string `json:"systemMessage"`
			} `json:"systemMessage"`
		} `json:"injectSteps"`
	}
	if exit != nil || stderr != "" || json.Unmarshal([]byte(stdout), &response) != nil || len(response.InjectSteps) != 1 {
		t.Fatalf("stdout=%q stderr=%q exit=%v", stdout, stderr, exit)
	}
	text := response.InjectSteps[0].SystemMessage.SystemMessage
	for _, expected := range []string{"mem_search", "mem_save", "ag-runtime", "event_id"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q in %q", expected, text)
		}
	}
}

func TestAntigravitySessionStartDegradesSafely(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer provider.Close()
	for _, input := range []string{`not json`, `{}`, `{"conversationId":"a","workspacePaths":["relative"]}`, `{"conversationId":"a","workspacePaths":["/one","/two"]}`} {
		if got := string(antigravitySessionStart([]byte(input), provider.URL)); got != "{}" {
			t.Fatalf("invalid input returned %s", got)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid workspace metadata caused requests")
	}
	input, _ := json.Marshal(map[string]any{"conversationId": "a", "workspacePaths": []string{t.TempDir()}})
	if got := string(antigravitySessionStart(input, provider.URL)); got != "{}" {
		t.Fatalf("unavailable server returned %s", got)
	}
}
