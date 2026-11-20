package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

// These tests call the workflow Create method directly.
// They need no Terraform CLI and no TF_ACC, so they run in `make test` and CI.

const createdWorkflowID = "f0b24de5-25e7-4fab-ab94-63d81db6c0c0"

// workflowCreateWire is the POST /rest/api/3/workflows/create body as Jira
// documents it. It is declared here, apart from the provider's own types, so
// the test checks the JSON field names the provider sends.
type workflowCreateWire struct {
	Scope struct {
		Type string `json:"type"`
	} `json:"scope"`
	Statuses []struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		Description     string `json:"description"`
		StatusCategory  string `json:"statusCategory"`
		StatusReference string `json:"statusReference"`
	} `json:"statuses"`
	Workflows []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Statuses    []struct {
			StatusReference string `json:"statusReference"`
		} `json:"statuses"`
		Transitions []struct {
			ID                string `json:"id"`
			Name              string `json:"name"`
			Type              string `json:"type"`
			ToStatusReference string `json:"toStatusReference"`
		} `json:"transitions"`
	} `json:"workflows"`
}

// workflowCreateMock serves the status lookup and the bulk create endpoint.
// The legacy POST /rest/api/3/workflow answers 405, as Jira Cloud does now.
type workflowCreateMock struct {
	mu            sync.Mutex
	statuses      map[string]map[string]interface{}
	statusQueries [][]string
	createBodies  []workflowCreateWire
}

func (m *workflowCreateMock) client(t *testing.T) *atlassian.Client {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/workflow":
			w.WriteHeader(http.StatusMethodNotAllowed)

		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/statuses":
			ids := r.URL.Query()["id"]
			m.statusQueries = append(m.statusQueries, ids)
			found := []map[string]interface{}{}
			for _, id := range ids {
				if s, ok := m.statuses[id]; ok {
					found = append(found, s)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(found) //nolint:errcheck

		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/workflows/create":
			var body workflowCreateWire
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			m.createBodies = append(m.createBodies, body)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
				"workflows": []interface{}{
					map[string]interface{}{"id": createdWorkflowID, "name": body.Workflows[0].Name},
				},
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	client, err := atlassian.NewClient(atlassian.ClientConfig{
		URL:   server.URL,
		User:  "test@test.com",
		Token: "test-token",
	})
	if err != nil {
		t.Fatalf("creating client: %s", err)
	}
	return client
}

// createWorkflow runs the resource's Create with a plan for the given statuses.
func createWorkflow(t *testing.T, r *workflowResource, name, description string, statusIDs []string) resource.CreateResponse {
	t.Helper()
	ctx := context.Background()

	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	tfType := schemaResp.Schema.Type().TerraformType(ctx)

	statuses, diags := types.ListValueFrom(ctx, types.StringType, statusIDs)
	if diags.HasError() {
		t.Fatalf("building statuses: %v", diags)
	}

	plan := tfsdk.Plan{Schema: schemaResp.Schema, Raw: tftypes.NewValue(tfType, nil)}
	diags = plan.Set(ctx, &workflowResourceModel{
		ID:          types.StringUnknown(),
		Name:        types.StringValue(name),
		Description: types.StringValue(description),
		Statuses:    statuses,
	})
	if diags.HasError() {
		t.Fatalf("building plan: %v", diags)
	}

	resp := resource.CreateResponse{
		State: tfsdk.State{Schema: schemaResp.Schema, Raw: tftypes.NewValue(tfType, nil)},
	}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
	return resp
}

// TestWorkflowResourceCreate_UsesBulkCreateAPI creates a workflow from two
// existing statuses. It checks the request Jira gets and the state that results.
func TestWorkflowResourceCreate_UsesBulkCreateAPI(t *testing.T) {
	mock := &workflowCreateMock{statuses: map[string]map[string]interface{}{
		"10000": {"id": "10000", "name": "To Do", "description": "Ready to start", "statusCategory": "TODO"},
		"10001": {"id": "10001", "name": "Doing", "description": "", "statusCategory": "IN_PROGRESS"},
	}}
	r := &workflowResource{client: mock.client(t)}

	resp := createWorkflow(t, r, "tf-test-workflow", "A test workflow", []string{"10001", "10000"})
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	if len(mock.statusQueries) != 1 || !slices.Equal(mock.statusQueries[0], []string{"10001", "10000"}) {
		t.Errorf("status lookups = %v, want one lookup of [10001 10000]", mock.statusQueries)
	}
	if len(mock.createBodies) != 1 {
		t.Fatalf("create calls = %d, want 1", len(mock.createBodies))
	}
	body := mock.createBodies[0]

	if body.Scope.Type != "GLOBAL" {
		t.Errorf("scope.type = %q, want GLOBAL", body.Scope.Type)
	}

	if len(body.Statuses) != 2 {
		t.Fatalf("statuses = %d, want 2", len(body.Statuses))
	}
	doing, todo := body.Statuses[0], body.Statuses[1]
	if doing.ID != "10001" || doing.Name != "Doing" || doing.StatusCategory != "IN_PROGRESS" || doing.StatusReference != "10001" {
		t.Errorf("statuses[0] = %+v, want the unchanged Doing status with reference 10001", doing)
	}
	if todo.ID != "10000" || todo.Name != "To Do" || todo.Description != "Ready to start" ||
		todo.StatusCategory != "TODO" || todo.StatusReference != "10000" {
		t.Errorf("statuses[1] = %+v, want the unchanged To Do status with reference 10000", todo)
	}

	if len(body.Workflows) != 1 {
		t.Fatalf("workflows = %d, want 1", len(body.Workflows))
	}
	wf := body.Workflows[0]
	if wf.Name != "tf-test-workflow" || wf.Description != "A test workflow" {
		t.Errorf("workflow name/description = %q/%q", wf.Name, wf.Description)
	}
	refs := make([]string, len(wf.Statuses))
	for i, s := range wf.Statuses {
		refs[i] = s.StatusReference
	}
	if !slices.Equal(refs, []string{"10001", "10000"}) {
		t.Errorf("workflow status references = %v, want [10001 10000]", refs)
	}
	if len(wf.Transitions) != 1 {
		t.Fatalf("transitions = %d, want 1", len(wf.Transitions))
	}
	if tr := wf.Transitions[0]; tr.Type != "INITIAL" || tr.ToStatusReference != "10001" || tr.ID == "" || tr.Name == "" {
		t.Errorf("transition = %+v, want an INITIAL transition into 10001", tr)
	}

	var got workflowResourceModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("reading state: %v", diags)
	}
	if got.ID.ValueString() != createdWorkflowID {
		t.Errorf("id = %q, want %q", got.ID.ValueString(), createdWorkflowID)
	}
	var gotStatuses []string
	got.Statuses.ElementsAs(context.Background(), &gotStatuses, false)
	if !slices.Equal(gotStatuses, []string{"10001", "10000"}) {
		t.Errorf("state statuses = %v, want [10001 10000]", gotStatuses)
	}
}

// TestWorkflowResourceCreate_UnknownStatus stops before creating anything when
// a configured status does not exist.
func TestWorkflowResourceCreate_UnknownStatus(t *testing.T) {
	mock := &workflowCreateMock{statuses: map[string]map[string]interface{}{
		"10000": {"id": "10000", "name": "To Do", "statusCategory": "TODO"},
	}}
	r := &workflowResource{client: mock.client(t)}

	resp := createWorkflow(t, r, "tf-test-workflow", "", []string{"10000", "99999"})
	if !resp.Diagnostics.HasError() {
		t.Fatal("Create succeeded, want an error for the unknown status")
	}
	if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, `"99999"`) {
		t.Errorf("error detail = %q, want it to name status 99999", detail)
	}
	if len(mock.createBodies) != 0 {
		t.Errorf("create calls = %d, want 0", len(mock.createBodies))
	}
}

// TestWorkflowResourceCreate_BatchesStatusLookups looks up 51 statuses in two
// calls, because GET /rest/api/3/statuses takes at most 50 IDs.
func TestWorkflowResourceCreate_BatchesStatusLookups(t *testing.T) {
	mock := &workflowCreateMock{statuses: map[string]map[string]interface{}{}}
	ids := make([]string, statusLookupBatch+1)
	for i := range ids {
		id := strconv.Itoa(10000 + i)
		ids[i] = id
		mock.statuses[id] = map[string]interface{}{"id": id, "name": "Status " + id, "statusCategory": "TODO"}
	}
	r := &workflowResource{client: mock.client(t)}

	resp := createWorkflow(t, r, "tf-test-workflow", "", ids)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	if len(mock.statusQueries) != 2 || len(mock.statusQueries[0]) != statusLookupBatch || len(mock.statusQueries[1]) != 1 {
		sizes := make([]int, len(mock.statusQueries))
		for i, q := range mock.statusQueries {
			sizes[i] = len(q)
		}
		t.Errorf("status lookup sizes = %v, want [50 1]", sizes)
	}
	if len(mock.createBodies) != 1 || len(mock.createBodies[0].Statuses) != len(ids) {
		t.Errorf("create calls = %d, want 1 with %d statuses", len(mock.createBodies), len(ids))
	}
}
