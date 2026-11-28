package jira

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// FakeScreenTabs serves Jira's screen tab field endpoints from memory: list,
// add, remove and move. Tabs exist only once SetFields seeds them. It is
// exported so the acceptance tests in package jira_test can use it too.
type FakeScreenTabs struct {
	t      testing.TB
	server *httptest.Server

	mu              sync.Mutex
	fields          map[string][]string // "{screenID}/{tabID}" → field IDs in display order
	writes          []string
	ignoreMoves     bool
	reorderOnDelete func([]string) []string
}

// NewFakeScreenTabs starts the fake and stops it when the test ends.
func NewFakeScreenTabs(t testing.TB) *FakeScreenTabs {
	t.Helper()
	f := &FakeScreenTabs{t: t, fields: map[string][]string{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

// URL is the base URL to point the provider at.
func (f *FakeScreenTabs) URL() string {
	return f.server.URL
}

// SetFields creates the tab if needed and sets its fields.
func (f *FakeScreenTabs) SetFields(screenID, tabID string, fieldIDs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fields[screenID+"/"+tabID] = slices.Clone(fieldIDs)
}

// Fields returns the tab's fields in display order.
func (f *FakeScreenTabs) Fields(screenID, tabID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.fields[screenID+"/"+tabID])
}

// Writes returns every add, remove and move served so far, as "METHOD path body".
func (f *FakeScreenTabs) Writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// ResetWrites forgets the writes served so far.
func (f *FakeScreenTabs) ResetWrites() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = nil
}

// IgnoreMoves makes the fake accept moves without changing the order.
func (f *FakeScreenTabs) IgnoreMoves() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ignoreMoves = true
}

// ReorderOnDelete makes the fake pass a tab's remaining fields through reorder
// after each removal, the way Jira renumbers fields that share a position.
func (f *FakeScreenTabs) ReorderOnDelete(reorder func([]string) []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reorderOnDelete = reorder
}

func (f *FakeScreenTabs) serve(w http.ResponseWriter, r *http.Request) {
	// /rest/api/3/screens/{screenId}/tabs/{tabId}/fields[/{fieldId}[/move]]
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/rest/api/3/screens/"), "/")
	if !strings.HasPrefix(r.URL.Path, "/rest/api/3/screens/") || len(parts) < 4 || parts[1] != "tabs" || parts[3] != "fields" {
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		return
	}
	if len(r.URL.RawQuery) > 0 {
		http.Error(w, "unexpected query "+r.URL.RawQuery, http.StatusBadRequest)
		return
	}

	key := parts[0] + "/" + parts[2]
	var body map[string]string
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	current, ok := f.fields[key]
	if !ok {
		http.Error(w, `{"errorMessages":["The screen tab was not found."]}`, http.StatusNotFound)
		return
	}

	switch {
	case r.Method == http.MethodGet && len(parts) == 4:
		list := make([]map[string]string, 0, len(current))
		for _, id := range current {
			list = append(list, map[string]string{"id": id, "name": "Name of " + id})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list) //nolint:errcheck

	case r.Method == http.MethodPost && len(parts) == 4:
		fieldID := body["fieldId"]
		if fieldID == "" || len(body) != 1 {
			http.Error(w, fmt.Sprintf("body must be {\"fieldId\": ...}, got %v", body), http.StatusBadRequest)
			return
		}
		if f.onScreen(parts[0], fieldID) {
			http.Error(w, `{"errorMessages":["Field already exists on the screen."]}`, http.StatusBadRequest)
			return
		}
		f.fields[key] = append(current, fieldID)
		f.writes = append(f.writes, fmt.Sprintf("POST %s %s", r.URL.Path, fieldID))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": fieldID, "name": "Name of " + fieldID}) //nolint:errcheck

	case r.Method == http.MethodDelete && len(parts) == 5:
		i := slices.Index(current, parts[4])
		if i < 0 {
			http.Error(w, `{"errorMessages":["The field was not found on the screen tab."]}`, http.StatusNotFound)
			return
		}
		remaining := slices.Delete(current, i, i+1)
		if f.reorderOnDelete != nil {
			remaining = f.reorderOnDelete(slices.Clone(remaining))
		}
		f.fields[key] = remaining
		f.writes = append(f.writes, "DELETE "+r.URL.Path)
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodPost && len(parts) == 6 && parts[5] == "move":
		i := slices.Index(current, parts[4])
		if i < 0 {
			http.Error(w, `{"errorMessages":["The field was not found on the screen tab."]}`, http.StatusNotFound)
			return
		}
		moved := slices.Delete(slices.Clone(current), i, i+1)
		switch {
		case len(body) == 1 && body["position"] == "First":
			moved = slices.Insert(moved, 0, parts[4])
		case len(body) == 1 && body["after"] != "":
			j := slices.Index(moved, body["after"])
			if j < 0 {
				http.Error(w, "after names a field that is not on the tab: "+body["after"], http.StatusBadRequest)
				return
			}
			moved = slices.Insert(moved, j+1, parts[4])
		default:
			http.Error(w, fmt.Sprintf("body must set after or position=First, got %v", body), http.StatusBadRequest)
			return
		}
		if !f.ignoreMoves {
			f.fields[key] = moved
		}
		f.writes = append(f.writes, fmt.Sprintf("POST %s %v", r.URL.Path, body))
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusMethodNotAllowed)
	}
}

// onScreen reports whether any tab of the screen holds the field. Jira allows a field on one tab per screen.
func (f *FakeScreenTabs) onScreen(screenID, fieldID string) bool {
	for key, ids := range f.fields {
		if strings.HasPrefix(key, screenID+"/") && slices.Contains(ids, fieldID) {
			return true
		}
	}
	return false
}
