package atlassian

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// taskServer answers GET /rest/api/3/task/10010 with the given statuses in
// turn, repeating the last one.
func taskServer(t *testing.T, statuses ...string) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/rest/api/3/task/10010" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		n := int(calls.Add(1)) - 1
		if n >= len(statuses) {
			n = len(statuses) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
			"status": statuses[n], "message": "status " + statuses[n],
		})
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ClientConfig{URL: server.URL, User: "user@example.com", Token: "token", Version: "test"})
	if err != nil {
		t.Fatalf("creating client: %s", err)
	}
	return client, &calls
}

func shortTaskPolls(t *testing.T) {
	t.Helper()
	interval := taskPollInterval
	taskPollInterval = time.Millisecond
	t.Cleanup(func() { taskPollInterval = interval })
}

func TestWaitForTask_PollsUntilComplete(t *testing.T) {
	shortTaskPolls(t)
	client, calls := taskServer(t, "ENQUEUED", "RUNNING", "COMPLETE")

	if err := client.WaitForTask(context.Background(), "10010"); err != nil {
		t.Fatalf("WaitForTask: %s", err)
	}
	if calls.Load() != 3 {
		t.Errorf("polled %d times, want 3", calls.Load())
	}
}

func TestWaitForTask_Failed(t *testing.T) {
	shortTaskPolls(t)
	client, _ := taskServer(t, "RUNNING", "FAILED")

	err := client.WaitForTask(context.Background(), "10010")
	if err == nil || !strings.Contains(err.Error(), "FAILED") || !strings.Contains(err.Error(), "status FAILED") {
		t.Errorf("err = %v, want the failed status and its message", err)
	}
}

func TestWaitForTask_ContextCancelled(t *testing.T) {
	interval := taskPollInterval
	taskPollInterval = time.Hour
	t.Cleanup(func() { taskPollInterval = interval })
	client, _ := taskServer(t, "RUNNING")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := client.WaitForTask(ctx, "10010"); err == nil {
		t.Error("WaitForTask returned nil, want the context's error")
	}
}

func TestWaitForTask_TimeoutDuringRequest(t *testing.T) {
	timeout := taskTimeout
	taskTimeout = 50 * time.Millisecond
	t.Cleanup(func() { taskTimeout = timeout })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/rest/api/3/task/10010" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"status":"COMPLETE"}`))
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{URL: server.URL, User: "user@example.com", Token: "token", Version: "test"})
	if err != nil {
		t.Fatalf("creating client: %s", err)
	}

	err = client.WaitForTask(context.Background(), "10010")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitForTask error = %v, want deadline exceeded", err)
	}
}
