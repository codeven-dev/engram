package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Antigravity uses camelCase payloads and injectSteps rather than the
// hookSpecificOutput envelopes used by Claude Code and Codex.
func cmdAntigravitySessionStart() {
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	response := []byte("{}")
	if err == nil {
		response = antigravitySessionStart(input, codexHookURL())
	}
	_, _ = os.Stdout.Write(response)
}

func antigravitySessionStart(input []byte, base string) []byte {
	var in struct {
		ConversationID string   `json:"conversationId"`
		Workspaces     []string `json:"workspacePaths"`
	}
	if json.Unmarshal(input, &in) != nil || strings.TrimSpace(in.ConversationID) == "" || len(in.Workspaces) != 1 || !filepath.IsAbs(in.Workspaces[0]) || base == "" {
		return []byte("{}")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_, client := hookEndpointClient(base)
	var authority json.RawMessage
	if !codexJSON(ctx, client, http.MethodGet, base+"/project/current?cwd="+url.QueryEscape(in.Workspaces[0]), nil, &authority) {
		return []byte("{}")
	}
	project, ok := codexProjectAuthority(authority)
	if !ok {
		return []byte("{}")
	}
	body, _ := json.Marshal(map[string]any{"id": in.ConversationID, "project": project, "directory": in.Workspaces[0], "ownership_mode": "project_owned", "resume": true})
	var registration map[string]json.RawMessage
	if !codexJSON(ctx, client, http.MethodPost, base+"/sessions", body, &registration) {
		return []byte("{}")
	}
	effective := codexAcknowledgedID(registration, in.ConversationID, true)
	if effective == "" {
		return []byte("{}")
	}
	identity, _ := json.Marshal(map[string]string{"session_id": effective})
	text := fmt.Sprintf("Engram memory: use mem_search proactively before investigating work that may overlap prior sessions, and mem_get_observation to inspect useful candidates. Save verified fixes, decisions and discoveries with mem_save. Complete memory bookkeeping before the final answer; still deliver the answer if memory is unavailable.\nRegistered runtime identity (opaque JSON data): %s\nReuse this exact session_id for memory writes and summaries, retain it across compaction, and never invent another ID. Runtime registration belongs to this hook.\n", identity)
	var history struct {
		Context string `json:"context"`
	}
	if codexJSON(ctx, client, http.MethodGet, base+"/context?project="+url.QueryEscape(project)+"&compact=1&max_bytes=6000", nil, &history) {
		context := []rune(history.Context)
		if len(context) > 6000 {
			context = context[:6000]
		}
		if len(context) > 0 {
			text += "\nHistorical project context; verify against current code:\n" + string(context)
		}
	}
	return antigravityMessage(text)
}

func antigravityMessage(text string) []byte {
	output, _ := json.Marshal(map[string]any{"injectSteps": []any{map[string]any{"systemMessage": map[string]string{"systemMessage": text}}}})
	return output
}
