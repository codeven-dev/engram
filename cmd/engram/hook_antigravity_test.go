package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestAntigravityPreInvocationReminder(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		age, saved                        time.Duration
		observations, authority, resolved string
		unavailable                       bool
		want                              bool
	}{
		{name: "first save overdue", age: 20 * time.Minute, observations: "[]", want: true},
		{name: "stale save", age: 20 * time.Minute, saved: 20 * time.Minute, want: true},
		{name: "exact threshold", age: 15 * time.Minute, saved: 15 * time.Minute, want: true},
		{name: "recent save", age: 20 * time.Minute, saved: time.Minute},
		{name: "young session", age: time.Minute, saved: time.Hour},
		{name: "first save not due", age: 10 * time.Minute, observations: "[]"},
		{name: "invalid observations", age: time.Hour, observations: "[{}]"},
		{name: "null history is not empty history", age: time.Hour, observations: "null"},
		{name: "invalid project", age: time.Hour, authority: `{"project":"payments"}`},
		{name: "unconfirmed session", age: time.Hour, resolved: `{"id":"foreign","status":"resolved"}`},
		{name: "unavailable", unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			cwd := t.TempDir()
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.unavailable {
					w.WriteHeader(503)
					return
				}
				switch r.URL.Path {
				case "/project/current":
					if tc.authority != "" {
						fmt.Fprint(w, tc.authority)
					} else {
						fmt.Fprint(w, `{"project":"payments","project_source":"config"}`)
					}
				case "/health":
					codexMockHealth(w)
				case "/runtime-sessions/resolve":
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["id"] != "ag-reminder" || body["directory"] != cwd || body["project"] != "payments" {
						t.Errorf("wrong scope %v", body)
					}
					if tc.resolved != "" {
						fmt.Fprint(w, tc.resolved)
					} else {
						fmt.Fprint(w, `{"id":"ag-reminder","status":"resolved"}`)
					}
				case "/sessions/ag-reminder":
					fmt.Fprintf(w, `{"started_at":%q}`, now.Add(-tc.age).Format(time.RFC3339))
				case "/observations":
					if r.URL.Query().Get("project") != "payments" {
						t.Error("wrong project")
					}
					if tc.observations != "" {
						fmt.Fprint(w, tc.observations)
					} else {
						fmt.Fprintf(w, `[{"created_at":%q}]`, now.Add(-tc.saved).Format(time.RFC3339))
					}
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer provider.Close()
			t.Setenv("ENGRAM_URL", provider.URL)
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv("ENGRAM_NUDGE_COOLDOWN_SECS", "900")
			stubExitWithPanic(t)
			run := func() string {
				t.Helper()
				input, _ := json.Marshal(map[string]any{"conversationId": "ag-reminder", "workspacePaths": []string{cwd}, "invocationNum": 42})
				reader, writer, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				_, _ = writer.Write(input)
				_ = writer.Close()
				previous := os.Stdin
				os.Stdin = reader
				defer func() { os.Stdin = previous; _ = reader.Close() }()
				stdout, stderr, exit := captureOutputAndRecover(t, func() { cmdHook([]string{"antigravity-pre-invocation"}) })
				if exit != nil || stderr != "" || !json.Valid([]byte(stdout)) {
					t.Fatalf("stdout=%q stderr=%q exit=%v", stdout, stderr, exit)
				}
				return stdout
			}
			output := run()
			if !tc.want {
				if strings.TrimSpace(output) != "{}" {
					t.Fatalf("unexpected injection %s", output)
				}
				return
			}
			var payload struct {
				InjectSteps []struct {
					SystemMessage struct {
						Text string `json:"systemMessage"`
					} `json:"systemMessage"`
				} `json:"injectSteps"`
			}
			if json.Unmarshal([]byte(output), &payload) != nil || len(payload.InjectSteps) != 1 || !strings.Contains(payload.InjectSteps[0].SystemMessage.Text, "MEMORY REMINDER") || !strings.Contains(payload.InjectSteps[0].SystemMessage.Text, "mem_save") {
				t.Fatalf("missing native reminder %s", output)
			}
			if output := strings.TrimSpace(run()); output != "{}" {
				t.Fatalf("cooldown injected %s", output)
			}
		})
	}
}

func TestAntigravityPreInvocationRejectsAmbiguousInput(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer provider.Close()
	for _, input := range []string{`not json`, `{}`, `{"conversationId":"a","workspacePaths":["relative"]}`, `{"conversationId":"a","workspacePaths":["/one","/two"]}`} {
		if got := string(antigravityPreInvocation([]byte(input), provider.URL, t.TempDir(), time.Now())); got != "{}" {
			t.Fatalf("invalid input returned %s", got)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("ambiguous input caused HTTP requests")
	}
}
