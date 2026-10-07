package claude_test

import (
	"encoding/json"
	"testing"

	"github.com/sebnow/orchestrator/internal/harness/claude"
)

func TestGivenTextAndUUIDWhenEncodingUserMessageThenLineMatchesTheStdinFormat(t *testing.T) {
	got := claude.EncodeUserMessage("Run the tests.", "5f0c6c1e-2b7a-4d7e-9a37-0c1d2e3f4a5b")

	want := `{"type":"user","message":{"role":"user","content":"Run the tests."},"uuid":"5f0c6c1e-2b7a-4d7e-9a37-0c1d2e3f4a5b"}` + "\n"
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGivenNoUUIDWhenEncodingUserMessageThenLineMatchesTheSpikeDriver(t *testing.T) {
	got := claude.EncodeUserMessage("Resume the task from where you stopped and finish it.", "")

	want := `{"type":"user","message":{"role":"user","content":"Resume the task from where you stopped and finish it."}}` + "\n"
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGivenTextWithQuotesAndNewlinesWhenEncodingUserMessageThenItIsOneLineThatDecodesBack(t *testing.T) {
	text := "line one\nsay \"hi\" <now> & then\tstop"

	got := claude.EncodeUserMessage(text, "u-1")

	body := got[:len(got)-1]
	for _, b := range body {
		if b == '\n' {
			t.Fatalf("encoded line contains a raw newline: %q", got)
		}
	}
	var decoded struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Priority *string `json:"priority"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Message.Content != text {
		t.Errorf("content = %q, want %q", decoded.Message.Content, text)
	}
	if decoded.Priority != nil {
		t.Errorf("priority = %q, want absent", *decoded.Priority)
	}
}

func TestGivenRequestIDWhenEncodingInterruptThenLineMatchesTheSpikeDriver(t *testing.T) {
	got := claude.EncodeInterrupt("spike-int-1")

	want := `{"type":"control_request","request_id":"spike-int-1","request":{"subtype":"interrupt"}}` + "\n"
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
