package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// capture swaps stdout for a pipe, so a test can read exactly what the command
// printed — including the last byte.
func capture(t *testing.T, run func()) string {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	saved := os.Stdout
	os.Stdout = writer

	done := make(chan string)

	go func() {
		var buffer bytes.Buffer
		_, _ = io.Copy(&buffer, reader)
		done <- buffer.String()
	}()

	run()

	os.Stdout = saved
	_ = writer.Close()

	out := <-done
	_ = reader.Close()

	return out
}

func TestCommandPrintsTheReportAndNothingElse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><head><title>Тест</title></head><body><h1>Тест</h1></body></html>")
	}))
	defer server.Close()

	out := capture(t, func() {
		if err := newCommand().Run(context.Background(), []string{"hexlet-go-crawler", "--depth", "1", server.URL}); err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	if len(out) == 0 || out[len(out)-1] != '\n' {
		t.Fatalf("output does not end with a newline: %q", out)
	}

	body := out[:len(out)-1]

	if !json.Valid([]byte(body)) {
		t.Fatalf("output is not JSON: %q", body)
	}

	if body[0] != '{' || body[len(body)-1] != '}' {
		t.Fatalf("something was printed around the JSON: %q", body)
	}

	var report struct {
		RootURL string `json:"root_url"`
		Pages   []struct {
			URL string `json:"url"`
		} `json:"pages"`
	}

	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if report.RootURL != server.URL || len(report.Pages) != 1 {
		t.Fatalf("report = %+v", report)
	}
}
