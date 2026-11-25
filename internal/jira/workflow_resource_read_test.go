package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

// These tests call the workflow resource's Read and ImportState
// directly against a mock of the bulk read, POST /rest/api/3/workflows.
// They need no Terraform CLI and no TF_ACC, so they run in `make test` and CI.

const fixtureWorkflowID = "4d8f5a78-e5e2-4357-8757-5b04a3c719e7"
const fixtureWorkflowName = "tf-test-workflow-transitions"

// workflowReadMock answers the bulk read with a fixture when the request names
// the fixture's workflow by ID or name, and 404 otherwise. Any other request
// fails the test.
type workflowReadMock struct {
	t        *testing.T
	fixture  []byte
	mu       sync.Mutex
	requests []workflowReadRequest
}

func (m *workflowReadMock) client() *atlassian.Client {
	m.t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rest/api/3/workflows" {
			m.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body workflowReadRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.requests = append(m.requests, body)
		m.mu.Unlock()

		if m.fixture == nil ||
			!(slices.Contains(body.WorkflowIDs, fixtureWorkflowID) || slices.Contains(body.WorkflowNames, fixtureWorkflowName)) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errorMessages":["Not found"],"errors":{}}`)) //nolint:errcheck
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(m.fixture) //nolint:errcheck
	}))
	m.t.Cleanup(server.Close)

	client, err := atlassian.NewClient(atlassian.ClientConfig{URL: server.URL, User: "test@test.com", Token: "test-token"})
	if err != nil {
		m.t.Fatalf("creating client: %s", err)
	}
	return client
}

func readFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/workflow_bulk_read.json")
	if err != nil {
		t.Fatalf("reading fixture: %s", err)
	}
	return data
}

// nestFixtureConditions puts one more condition group under the deepest group
// of transition 21, making the conditions 4 levels deep.
func nestFixtureConditions(t *testing.T, fixture []byte) []byte {
	t.Helper()
	var doc map[string]interface{}
	if err := json.Unmarshal(fixture, &doc); err != nil {
		t.Fatal(err)
	}
	wf := doc["workflows"].([]interface{})[0].(map[string]interface{})
	for _, tr := range wf["transitions"].([]interface{}) {
		tm := tr.(map[string]interface{})
		if tm["id"] != "21" {
			continue
		}
		level2 := tm["conditions"].(map[string]interface{})["conditionGroups"].([]interface{})[0].(map[string]interface{})
		level3 := level2["conditionGroups"].([]interface{})[0].(map[string]interface{})
		level3["conditionGroups"] = []interface{}{map[string]interface{}{
			"operation":       "ANY",
			"conditions":      []interface{}{map[string]interface{}{"ruleKey": "system:restrict-issue-transition", "parameters": map[string]interface{}{}}},
			"conditionGroups": []interface{}{},
		}}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func workflowSchema(t *testing.T) (resource.SchemaResponse, tftypes.Type) {
	t.Helper()
	var resp resource.SchemaResponse
	(&workflowResource{}).Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema implementation: %v", diags)
	}
	return resp, resp.Schema.Type().TerraformType(context.Background())
}

// readWorkflowState runs Read from a prior state with the given ID and statuses.
func readWorkflowState(t *testing.T, r *workflowResource, id string, statuses []string) resource.ReadResponse {
	t.Helper()
	ctx := context.Background()
	schemaResp, tfType := workflowSchema(t)

	statusList := types.ListNull(types.StringType)
	if statuses != nil {
		statusList, _ = types.ListValueFrom(ctx, types.StringType, statuses)
	}
	prior := tfsdk.State{Schema: schemaResp.Schema, Raw: tftypes.NewValue(tfType, nil)}
	if diags := prior.Set(ctx, &workflowResourceModel{
		ID:          types.StringValue(id),
		Name:        types.StringValue(fixtureWorkflowName),
		Description: types.StringValue(""),
		Statuses:    statusList,
		Transitions: types.MapNull(workflowTransitionObjectType()),
		Version:     types.ObjectNull(workflowVersionAttrTypes),
	}); diags.HasError() {
		t.Fatalf("building prior state: %v", diags)
	}

	resp := resource.ReadResponse{State: tfsdk.State{Schema: schemaResp.Schema, Raw: prior.Raw.Copy()}}
	r.Read(ctx, resource.ReadRequest{State: prior}, &resp)
	return resp
}

func strPtr(s string) *string { return &s }

// TestWorkflowResourceRead_Transitions reads every transition field from the
// bulk read: status references become status IDs, rule IDs and diagram layout
// are dropped, and an empty condition group reads as no conditions.
func TestWorkflowResourceRead_Transitions(t *testing.T) {
	mock := &workflowReadMock{t: t, fixture: readFixture(t)}
	r := &workflowResource{client: mock.client()}

	resp := readWorkflowState(t, r, fixtureWorkflowID, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}
	if len(mock.requests) != 1 || !slices.Equal(mock.requests[0].WorkflowIDs, []string{fixtureWorkflowID}) {
		t.Errorf("requests = %+v, want one read by ID", mock.requests)
	}

	ctx := context.Background()
	var model workflowResourceModel
	if diags := resp.State.Get(ctx, &model); diags.HasError() {
		t.Fatalf("reading state: %v", diags)
	}
	if model.ID.ValueString() != fixtureWorkflowID || model.Name.ValueString() != fixtureWorkflowName ||
		model.Description.ValueString() != "Workflow with transitions" {
		t.Errorf("id/name/description = %s/%s/%s", model.ID, model.Name, model.Description)
	}

	var statuses []string
	model.Statuses.ElementsAs(ctx, &statuses, false)
	if want := []string{"10000", "3", "10002", "10100"}; !slices.Equal(statuses, want) {
		t.Errorf("statuses = %v, want %v", statuses, want)
	}

	var version workflowVersionValue
	if diags := model.Version.As(ctx, &version, basetypes.ObjectAsOptions{}); diags.HasError() {
		t.Fatalf("version: %v", diags)
	}
	if version.ID != "bc2123c5-88d6-4eeb-a498-7ebd5387a02a" || version.Number != 4 {
		t.Errorf("version = %+v", version)
	}

	var transitions map[string]workflowTransitionValue
	if diags := resp.State.GetAttribute(ctx, path.Root("transitions"), &transitions); diags.HasError() {
		t.Fatalf("transitions: %v", diags)
	}

	none := []workflowRuleValue{}
	restrict := func(params map[string]string) workflowRuleValue {
		return workflowRuleValue{RuleKey: "system:restrict-issue-transition", Parameters: params}
	}
	want := map[string]workflowTransitionValue{
		"1": {
			Name: "Create", Type: "INITIAL", From: []string{}, To: strPtr("10000"),
			Validators: []workflowRuleValue{{
				RuleKey:    "system:check-permission-validator",
				Parameters: map[string]string{"permissionKey": "CREATE_ISSUES"},
			}},
			Actions: none, Triggers: none,
			Properties: map[string]string{"jira.i18n.title": "common.forms.create"},
		},
		"21": {
			Name: "Request review", Description: "Send for review", Type: "DIRECTED",
			From: []string{"3"}, To: strPtr("10100"),
			Conditions: &workflowConditionGroup1{
				Operation: "ALL",
				Rules: []workflowRuleValue{restrict(map[string]string{
					"accountIds": "allow-assignee", "roleIds": "", "groupIds": "", "permissionKeys": "",
					"groupCustomFields": "", "allowUserCustomFields": "", "denyUserCustomFields": "",
				})},
				Groups: []workflowConditionGroup2{{
					Operation: "ANY",
					Rules:     []workflowRuleValue{restrict(map[string]string{"permissionKeys": "RESOLVE_ISSUES"})},
					Groups: []workflowConditionGroup3{{
						Operation: "ALL",
						Rules:     []workflowRuleValue{restrict(map[string]string{"permissionKeys": "CLOSE_ISSUES"})},
					}},
				}},
			},
			Validators: none, Actions: none,
			Triggers: []workflowRuleValue{{
				RuleKey:    "system:pull-request-created-trigger",
				Parameters: map[string]string{"enabled": "true"},
			}},
			Properties: map[string]string{},
		},
		"31": {
			Name: "Done", Type: "DIRECTED", From: []string{"10000", "10100", "3"}, To: strPtr("10002"),
			Validators: none, Triggers: none,
			Actions: []workflowRuleValue{{
				RuleKey:    "system:update-field",
				Parameters: map[string]string{"field": "resolution", "value": "10000", "mode": "replace"},
			}},
			Properties: map[string]string{},
		},
		"11": {
			Name: "Open", Type: "GLOBAL", From: []string{}, To: strPtr("10000"),
			Validators: none, Triggers: none,
			Actions: []workflowRuleValue{{
				RuleKey:    "system:update-field",
				Parameters: map[string]string{"field": "resolution", "value": "", "mode": ""},
			}},
			Properties: map[string]string{},
		},
	}
	for id, tr := range transitions {
		slices.Sort(tr.From)
		transitions[id] = tr
	}
	if !reflect.DeepEqual(transitions, want) {
		got, _ := json.MarshalIndent(transitions, "", "  ")
		t.Errorf("transitions =\n%s", got)
	}
}

// TestWorkflowResourceRead_KeepsStatusOrder keeps the prior state's status
// order when Jira holds the same statuses, and takes Jira's order otherwise.
func TestWorkflowResourceRead_KeepsStatusOrder(t *testing.T) {
	mock := &workflowReadMock{t: t, fixture: readFixture(t)}
	r := &workflowResource{client: mock.client()}
	ctx := context.Background()

	for _, tc := range []struct {
		prior, want []string
	}{
		{prior: []string{"10100", "10002", "3", "10000"}, want: []string{"10100", "10002", "3", "10000"}},
		{prior: []string{"10002", "3", "10000"}, want: []string{"10000", "3", "10002", "10100"}},
	} {
		resp := readWorkflowState(t, r, fixtureWorkflowID, tc.prior)
		if resp.Diagnostics.HasError() {
			t.Fatalf("Read: %v", resp.Diagnostics)
		}
		var got []string
		resp.State.GetAttribute(ctx, path.Root("statuses"), &got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("prior %v: statuses = %v, want %v", tc.prior, got, tc.want)
		}
	}
}

// TestWorkflowResourceRead_NotFound removes the resource when Jira answers 404.
func TestWorkflowResourceRead_NotFound(t *testing.T) {
	mock := &workflowReadMock{t: t}
	r := &workflowResource{client: mock.client()}

	resp := readWorkflowState(t, r, fixtureWorkflowID, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Error("state is not null, want the resource removed")
	}
}

// TestWorkflowResourceRead_ConditionsTooDeep fails, rather than drop levels,
// when the conditions nest deeper than the schema holds.
func TestWorkflowResourceRead_ConditionsTooDeep(t *testing.T) {
	mock := &workflowReadMock{t: t, fixture: nestFixtureConditions(t, readFixture(t))}
	r := &workflowResource{client: mock.client()}

	resp := readWorkflowState(t, r, fixtureWorkflowID, nil)
	if !resp.Diagnostics.HasError() {
		t.Fatal("Read succeeded, want an error for conditions 4 levels deep")
	}
	if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, "more than 3 levels") ||
		!strings.Contains(detail, "Request review") {
		t.Errorf("error detail = %q", detail)
	}
}

func importWorkflow(t *testing.T, r *workflowResource, id string) resource.ImportStateResponse {
	t.Helper()
	schemaResp, tfType := workflowSchema(t)
	resp := resource.ImportStateResponse{
		State: tfsdk.State{Schema: schemaResp.Schema, Raw: tftypes.NewValue(tfType, nil)},
	}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: id}, &resp)
	return resp
}

// TestWorkflowResourceImportState_ByIDOrName imports by entity ID with one
// read, and by name after the ID lookup finds nothing.
func TestWorkflowResourceImportState_ByIDOrName(t *testing.T) {
	for _, tc := range []struct {
		importID string
		want     []workflowReadRequest
	}{
		{importID: fixtureWorkflowID, want: []workflowReadRequest{{WorkflowIDs: []string{fixtureWorkflowID}}}},
		{importID: fixtureWorkflowName, want: []workflowReadRequest{
			{WorkflowIDs: []string{fixtureWorkflowName}},
			{WorkflowNames: []string{fixtureWorkflowName}},
		}},
	} {
		mock := &workflowReadMock{t: t, fixture: readFixture(t)}
		r := &workflowResource{client: mock.client()}

		resp := importWorkflow(t, r, tc.importID)
		if resp.Diagnostics.HasError() {
			t.Fatalf("import %q: %v", tc.importID, resp.Diagnostics)
		}
		var id string
		resp.State.GetAttribute(context.Background(), path.Root("id"), &id)
		if id != fixtureWorkflowID {
			t.Errorf("import %q: id = %q, want %q", tc.importID, id, fixtureWorkflowID)
		}
		if !reflect.DeepEqual(mock.requests, tc.want) {
			t.Errorf("import %q: requests = %+v, want %+v", tc.importID, mock.requests, tc.want)
		}
	}
}

// TestWorkflowResourceImportState_NotFound names the ID that matched nothing.
func TestWorkflowResourceImportState_NotFound(t *testing.T) {
	mock := &workflowReadMock{t: t}
	r := &workflowResource{client: mock.client()}

	resp := importWorkflow(t, r, "No Such Workflow")
	if !resp.Diagnostics.HasError() {
		t.Fatal("import succeeded, want an error")
	}
	if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, `"No Such Workflow"`) {
		t.Errorf("error detail = %q", detail)
	}
}

// TestWorkflowSchema_NeverReplaces checks that no attribute forces a
// replacement: replacing a workflow would delete a live one.
func TestWorkflowSchema_NeverReplaces(t *testing.T) {
	ctx := context.Background()
	schemaResp, _ := workflowSchema(t)
	for name, a := range schemaResp.Schema.Attributes {
		mods := reflect.ValueOf(a).FieldByName("PlanModifiers")
		if !mods.IsValid() {
			continue
		}
		for i := 0; i < mods.Len(); i++ {
			m, ok := mods.Index(i).Interface().(interface{ Description(context.Context) string })
			if !ok {
				t.Fatalf("%s: plan modifier %T has no Description", name, mods.Index(i).Interface())
			}
			if strings.Contains(m.Description(ctx), "destroy and recreate") {
				t.Errorf("%s: plan modifier forces a replacement: %s", name, m.Description(ctx))
			}
		}
	}
}
