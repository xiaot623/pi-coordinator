package runner

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type testWriteCloser struct {
	bytes.Buffer
}

func (w *testWriteCloser) Close() error { return nil }

func TestSplitModelRef(t *testing.T) {
	provider, modelID, err := splitModelRef("openrouter/openai/gpt-4o")
	if err != nil {
		t.Fatalf("splitModelRef returned error: %v", err)
	}
	if provider != "openrouter" || modelID != "openai/gpt-4o" {
		t.Fatalf("unexpected split result: provider=%q modelID=%q", provider, modelID)
	}
}

func TestSplitModelRefRejectsInvalidValue(t *testing.T) {
	if _, _, err := splitModelRef("invalid-model"); err == nil {
		t.Fatal("expected error for invalid model ref")
	}
}

func TestLocalProcessSendWithModelSwitchesBeforePrompt(t *testing.T) {
	writer := &testWriteCloser{}
	proc := &LocalProcess{stdin: writer, currentModel: "provider-a/model-a"}

	if err := proc.SendWithModel("provider-b/model-b", buildRPCPayload("prompt", "hello", nil)); err != nil {
		t.Fatalf("SendWithModel returned error: %v", err)
	}

	lines := bytes.Split(bytes.TrimSpace(writer.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected 2 payloads, got %d", len(lines))
	}

	var setModel map[string]any
	if err := json.Unmarshal(lines[0], &setModel); err != nil {
		t.Fatalf("failed to unmarshal set_model payload: %v", err)
	}
	if setModel["type"] != "set_model" || setModel["provider"] != "provider-b" || setModel["modelId"] != "model-b" {
		t.Fatalf("unexpected set_model payload: %#v", setModel)
	}

	var prompt map[string]any
	if err := json.Unmarshal(lines[1], &prompt); err != nil {
		t.Fatalf("failed to unmarshal prompt payload: %v", err)
	}
	if prompt["type"] != "prompt" || prompt["message"] != "hello" {
		t.Fatalf("unexpected prompt payload: %#v", prompt)
	}
	if proc.currentModel != "provider-b/model-b" {
		t.Fatalf("expected currentModel to update, got %q", proc.currentModel)
	}
}

func TestEnvWithOverridesAndSortsExtraKeys(t *testing.T) {
	env := envWith([]string{"B=old", "A=keep", "TELEGRAM_BOT_TOKEN=old-token"}, map[string]string{
		"TELEGRAM_CHAT_ID":   "123",
		"TELEGRAM_BOT_TOKEN": "new-token",
	})
	joined := strings.Join(env, "\n")
	if joined != "B=old\nA=keep\nTELEGRAM_BOT_TOKEN=new-token\nTELEGRAM_CHAT_ID=123" {
		t.Fatalf("unexpected env merge:\n%s", joined)
	}
}

func TestLocalProcessSendWithModelSkipsNoopSwitch(t *testing.T) {
	writer := &testWriteCloser{}
	proc := &LocalProcess{stdin: writer, currentModel: "provider-a/model-a"}

	if err := proc.SendWithModel("provider-a/model-a", buildRPCPayload("steer", "next", nil)); err != nil {
		t.Fatalf("SendWithModel returned error: %v", err)
	}

	lines := bytes.Split(bytes.TrimSpace(writer.Bytes()), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("expected 1 payload, got %d", len(lines))
	}

	var payload map[string]any
	if err := json.Unmarshal(lines[0], &payload); err != nil {
		t.Fatalf("failed to unmarshal payload: %v", err)
	}
	if payload["type"] != "steer" || payload["message"] != "next" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}
