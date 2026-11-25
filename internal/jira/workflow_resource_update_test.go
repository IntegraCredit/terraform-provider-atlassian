package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

// These tests call the workflow resource's Update and ModifyPlan directly
// against a mock of the bulk read, the status lookup, the bulk update and its
// validation. They need no Terraform CLI and no TF_ACC, so they run in
// `make test` and CI.

// workflowUpdateMock serves a workflow that starts as the update fixture and
// changes as Jira would on each update. It records every request.
type workflowUpdateMock struct {
	t                *testing.T
	mu               sync.Mutex
	doc              []byte
	validationErrors []map[string]interface{}
	taskID           string
	requests         []string
	validations      []json.RawMessage
	updates          []json.RawMessage
}

// updateStatuses are the statuses GET /rest/api/3/statuses knows: the
// fixture's four and a new one, 10200.
var updateStatuses = map[string]map[string]interface{}{
	"10000": {"id": "10000", "name": "Backlog", "description": "", "statusCategory": "TODO"},
	"3":     {"id": "3", "name": "In Progress", "description": "", "statusCategory": "IN_PROGRESS"},
	"10002": {"id": "10002", "name": "Done", "description": "", "statusCategory": "DONE"},
	"10100": {"id": "10100", "name": "Review", "description": "", "statusCategory": "IN_PROGRESS"},
	"10200": {"id": "10200", "name": "Merged", "description": "Merged into main", "statusCategory": "IN_PROGRESS"},
}

// updateFixture is the bulk read fixture with what the provider does not
// manage but must keep: a looped transition layout, a status property, and a
// transition screen and event on transition 31.
func updateFixture(t *testing.T) []byte {
	t.Helper()
	var doc map[string]interface{}
	if err := json.Unmarshal(readFixture(t), &doc); err != nil {
		t.Fatal(err)
	}
	wf := doc["workflows"].([]interface{})[0].(map[string]interface{})
	wf["loopedTransitionContainerLayout"] = map[string]interface{}{"x": -5, "y": 7}
	for _, s := range wf["statuses"].([]interface{}) {
		sm := s.(map[string]interface{})
		if sm["statusReference"] == "3" {
			sm["properties"] = map[string]interface{}{"jira.issue.editable": "false"}
		}
	}
	for _, tr := range wf["transitions"].([]interface{}) {
		tm := tr.(map[string]interface{})
		if tm["id"] == "31" {
			tm["transitionScreen"] = map[string]interface{}{
				"id": "a1b2c3d4-0000-4000-8000-000000000031", "ruleKey": "system:transition-screen",
				"parameters": map[string]interface{}{"screenId": "10050"},
			}
			tm["customIssueEventId"] = "10001"
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (m *workflowUpdateMock) client() *atlassian.Client {
	m.t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.requests = append(m.requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/workflows":
			var body workflowReadRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if !slices.Equal(body.WorkflowIDs, []string{fixtureWorkflowID}) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(m.doc) //nolint:errcheck

		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/statuses":
			found := []map[string]interface{}{}
			for _, id := range r.URL.Query()["id"] {
				if s, ok := updateStatuses[id]; ok {
					found = append(found, s)
				}
			}
			json.NewEncoder(w).Encode(found) //nolint:errcheck

		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/workflows/update/validation":
			var body struct {
				Payload           json.RawMessage `json:"payload"`
				ValidationOptions struct {
					Levels []string `json:"levels"`
				} `json:"validationOptions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if !slices.Equal(body.ValidationOptions.Levels, []string{"ERROR", "WARNING"}) {
				m.t.Errorf("validation levels = %v", body.ValidationOptions.Levels)
			}
			m.validations = append(m.validations, body.Payload)
			errs := m.validationErrors
			if errs == nil {
				errs = []map[string]interface{}{}
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"errors": errs}) //nolint:errcheck

		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/workflows/update":
			var body json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			m.updates = append(m.updates, body)
			doc, err := ApplyWorkflowUpdate(m.doc, body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			m.doc = doc
			var taskID interface{}
			if m.taskID != "" {
				taskID = m.taskID
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"taskId": taskID}) //nolint:errcheck

		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/task/"+m.taskID && m.taskID != "":
			json.NewEncoder(w).Encode(map[string]string{"status": "COMPLETE"}) //nolint:errcheck

		default:
			m.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	m.t.Cleanup(server.Close)

	client, err := atlassian.NewClient(atlassian.ClientConfig{URL: server.URL, User: "test@test.com", Token: "test-token"})
	if err != nil {
		m.t.Fatalf("creating client: %s", err)
	}
	return client
}

// newUpdateMock returns a mock serving the update fixture and the resource's
// state after reading it, as Terraform holds it before an update.
func newUpdateMock(t *testing.T) (*workflowUpdateMock, *workflowResource, tfsdk.State) {
	t.Helper()
	mock := &workflowUpdateMock{t: t, doc: updateFixture(t)}
	r := &workflowResource{client: mock.client()}
	read := readWorkflowState(t, r, fixtureWorkflowID, []string{"10000", "3", "10002", "10100"})
	if read.Diagnostics.HasError() {
		t.Fatalf("Read: %v", read.Diagnostics)
	}
	mock.requests = nil
	return mock, r, read.State
}

// updatePlan builds the plan for an update from the state: edit changes the
// model and the transitions. A nil transitions map plans null transitions.
// The version is unknown, as the framework plans it for any change.
func updatePlan(t *testing.T, state tfsdk.State, edit func(m *workflowResourceModel, tr map[string]workflowTransitionValue) map[string]workflowTransitionValue) tfsdk.Plan {
	t.Helper()
	ctx := context.Background()
	var model workflowResourceModel
	if diags := state.Get(ctx, &model); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	var transitions map[string]workflowTransitionValue
	if diags := state.GetAttribute(ctx, path.Root("transitions"), &transitions); diags.HasError() {
		t.Fatalf("transitions: %v", diags)
	}

	transitions = edit(&model, transitions)
	model.Transitions = types.MapNull(workflowTransitionObjectType())
	if transitions != nil {
		value, err := workflowTransitionsMapValue(ctx, transitions)
		if err != nil {
			t.Fatal(err)
		}
		model.Transitions = value
	}
	model.Version = types.ObjectUnknown(workflowVersionAttrTypes)

	plan := tfsdk.Plan{Schema: state.Schema, Raw: state.Raw.Copy()}
	if diags := plan.Set(ctx, &model); diags.HasError() {
		t.Fatalf("plan: %v", diags)
	}
	return plan
}

func runUpdate(r *workflowResource, state tfsdk.State, plan tfsdk.Plan) resource.UpdateResponse {
	resp := resource.UpdateResponse{State: tfsdk.State{Schema: state.Schema, Raw: plan.Raw.Copy()}}
	config := tfsdk.Config{Schema: plan.Schema, Raw: plan.Raw.Copy()}
	r.Update(context.Background(), resource.UpdateRequest{Plan: plan, State: state, Config: config}, &resp)
	return resp
}

func setStatuses(t *testing.T, m *workflowResourceModel, ids ...string) {
	t.Helper()
	list, diags := types.ListValueFrom(context.Background(), types.StringType, ids)
	if diags.HasError() {
		t.Fatalf("statuses: %v", diags)
	}
	m.Statuses = list
}

// addMergedStatus adds status 10200 with a transition into it, renames
// transition 31, and gives transition 11 a different post function.
func addMergedStatus(t *testing.T) func(m *workflowResourceModel, tr map[string]workflowTransitionValue) map[string]workflowTransitionValue {
	return func(m *workflowResourceModel, tr map[string]workflowTransitionValue) map[string]workflowTransitionValue {
		setStatuses(t, m, "10000", "3", "10002", "10100", "10200")

		done := tr["31"]
		done.Name = "Finish"
		tr["31"] = done

		open := tr["11"]
		open.Actions = []workflowRuleValue{{RuleKey: "system:change-assignee", Parameters: map[string]string{"type": "to-current-user"}}}
		tr["11"] = open

		tr["41"] = workflowTransitionValue{
			Name: "Merge", Type: "DIRECTED", From: []string{"10002"}, To: strPtr("10200"),
			Validators: []workflowRuleValue{}, Actions: []workflowRuleValue{}, Triggers: []workflowRuleValue{},
			Properties: map[string]string{},
		}
		return tr
	}
}

// requireJSONEqual compares two JSON documents by value.
func requireJSONEqual(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var g, w interface{}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("decoding got: %s", err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("decoding want: %s", err)
	}
	if !reflect.DeepEqual(g, w) {
		gotPretty, _ := json.MarshalIndent(g, "", "  ")
		wantPretty, _ := json.MarshalIndent(w, "", "  ")
		t.Errorf("request body differs\ngot:\n%s\nwant:\n%s", gotPretty, wantPretty)
	}
}

// wantMergedUpdate is the request for addMergedStatus. It names the live
// version, sends every status as Jira holds it, and keeps the layout, ports,
// status property, transition screen, event and the IDs of rules that stay.
// The new status and transition get no layout or ports, and the post
// function that changed key gets no ID. Transition 11's empty condition group
// is left out, as Jira reads it as no conditions.
const wantMergedUpdate = `{
  "statuses": [
    {"id": "10000", "name": "Backlog", "description": "", "statusCategory": "TODO", "statusReference": "10000"},
    {"id": "3", "name": "In Progress", "description": "", "statusCategory": "IN_PROGRESS", "statusReference": "3"},
    {"id": "10002", "name": "Done", "description": "", "statusCategory": "DONE", "statusReference": "10002"},
    {"id": "10100", "name": "Review", "description": "", "statusCategory": "IN_PROGRESS", "statusReference": "10100"},
    {"id": "10200", "name": "Merged", "description": "Merged into main", "statusCategory": "IN_PROGRESS", "statusReference": "10200"}
  ],
  "workflows": [{
    "id": "4d8f5a78-e5e2-4357-8757-5b04a3c719e7",
    "description": "Workflow with transitions",
    "version": {"id": "bc2123c5-88d6-4eeb-a498-7ebd5387a02a", "versionNumber": 4},
    "startPointLayout": {"x": 10, "y": 20},
    "loopedTransitionContainerLayout": {"x": -5, "y": 7},
    "statuses": [
      {"statusReference": "10000", "layout": {"x": 65.15625, "y": -117.6}, "properties": {}},
      {"statusReference": "3", "layout": {"x": 449.03, "y": 55.55}, "properties": {"jira.issue.editable": "false"}},
      {"statusReference": "10002", "layout": {"x": 800, "y": 0}, "properties": {}},
      {"statusReference": "10100", "layout": {"x": 600, "y": 100}, "properties": {}},
      {"statusReference": "10200", "properties": {}}
    ],
    "transitions": [
      {
        "id": "1", "name": "Create", "description": "", "type": "INITIAL", "toStatusReference": "10000",
        "links": [{"toPort": 7}],
        "validators": [{"id": "e2ee06bf-bf74-4237-b0c9-1d4d0aa0cffd", "ruleKey": "system:check-permission-validator", "parameters": {"permissionKey": "CREATE_ISSUES"}}],
        "actions": [], "triggers": [],
        "properties": {"jira.i18n.title": "common.forms.create"}
      },
      {
        "id": "11", "name": "Open", "description": "", "type": "GLOBAL", "toStatusReference": "10000",
        "links": [],
        "validators": [],
        "actions": [{"ruleKey": "system:change-assignee", "parameters": {"type": "to-current-user"}}],
        "triggers": [], "properties": {}
      },
      {
        "id": "21", "name": "Request review", "description": "Send for review", "type": "DIRECTED", "toStatusReference": "10100",
        "links": [{"fromStatusReference": "3", "fromPort": 4, "toPort": 2}],
        "conditions": {
          "operation": "ALL",
          "conditions": [{"id": "98877b23-ace0-4b88-89b5-b87030bf1f1e", "ruleKey": "system:restrict-issue-transition", "parameters": {"accountIds": "allow-assignee", "roleIds": "", "groupIds": "", "permissionKeys": "", "groupCustomFields": "", "allowUserCustomFields": "", "denyUserCustomFields": ""}}],
          "conditionGroups": [{
            "operation": "ANY",
            "conditions": [{"id": "3c73a845-0903-47aa-af43-47373cb5347c", "ruleKey": "system:restrict-issue-transition", "parameters": {"permissionKeys": "RESOLVE_ISSUES"}}],
            "conditionGroups": [{
              "operation": "ALL",
              "conditions": [{"id": "c812def2-af55-43ab-a708-b390eeb8b1b3", "ruleKey": "system:restrict-issue-transition", "parameters": {"permissionKeys": "CLOSE_ISSUES"}}],
              "conditionGroups": []
            }]
          }]
        },
        "validators": [], "actions": [],
        "triggers": [{"id": "0b6b1c2e-0d1a-4f7e-8d55-0f8f6a1f2b3c", "ruleKey": "system:pull-request-created-trigger", "parameters": {"enabled": "true"}}],
        "properties": {}
      },
      {
        "id": "31", "name": "Finish", "description": "", "type": "DIRECTED", "toStatusReference": "10002",
        "links": [
          {"fromStatusReference": "10000", "fromPort": 4, "toPort": 0},
          {"fromStatusReference": "10100", "fromPort": 4, "toPort": 0},
          {"fromStatusReference": "3", "fromPort": 5, "toPort": 1}
        ],
        "validators": [],
        "actions": [{"id": "b049fc12-bb76-49a8-ab3d-468c834bf95a", "ruleKey": "system:update-field", "parameters": {"field": "resolution", "value": "10000", "mode": "replace"}}],
        "triggers": [], "properties": {},
        "transitionScreen": {"id": "a1b2c3d4-0000-4000-8000-000000000031", "ruleKey": "system:transition-screen", "parameters": {"screenId": "10050"}},
        "customIssueEventId": "10001"
      },
      {
        "id": "41", "name": "Merge", "description": "", "type": "DIRECTED", "toStatusReference": "10200",
        "links": [{"fromStatusReference": "10002"}],
        "validators": [], "actions": [], "triggers": [], "properties": {}
      }
    ]
  }]
}`

// TestWorkflowResourceUpdate_SendsWholeWorkflow adds a status and a
// transition, renames a transition and changes a post function. It checks
// the request Jira validates and then gets, and the state that results.
func TestWorkflowResourceUpdate_SendsWholeWorkflow(t *testing.T) {
	mock, r, state := newUpdateMock(t)
	plan := updatePlan(t, state, addMergedStatus(t))

	resp := runUpdate(r, state, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	if len(mock.validations) != 1 || len(mock.updates) != 1 {
		t.Fatalf("validations/updates = %d/%d, want 1/1", len(mock.validations), len(mock.updates))
	}
	requireJSONEqual(t, mock.updates[0], wantMergedUpdate)
	requireJSONEqual(t, mock.validations[0], string(mock.updates[0]))

	wantOrder := []string{
		"POST /rest/api/3/workflows", "GET /rest/api/3/statuses", "POST /rest/api/3/workflows/update/validation",
		"POST /rest/api/3/workflows/update", "POST /rest/api/3/workflows",
	}
	if !slices.Equal(mock.requests, wantOrder) {
		t.Errorf("requests = %v, want %v", mock.requests, wantOrder)
	}

	// The state is the plan, with the version Jira gave the update.
	ctx := context.Background()
	var got, planned workflowResourceModel
	resp.State.Get(ctx, &got)
	plan.Get(ctx, &planned)
	var version workflowVersionValue
	if diags := got.Version.As(ctx, &version, basetypes.ObjectAsOptions{}); diags.HasError() {
		t.Fatalf("version: %v", diags)
	}
	if version.Number != 5 || version.ID != "00000000-0000-0000-0000-000000000005" {
		t.Errorf("version = %+v, want number 5", version)
	}
	if !got.Statuses.Equal(planned.Statuses) || !got.Description.Equal(planned.Description) || !got.Name.Equal(planned.Name) {
		t.Errorf("state = %v/%v/%v, want the plan's", got.Name, got.Description, got.Statuses)
	}
	if !got.Transitions.Equal(planned.Transitions) {
		t.Errorf("transitions in state differ from the plan\nstate: %v\nplan:  %v", got.Transitions, planned.Transitions)
	}
}

// TestWorkflowResourceUpdate_ValidationErrorsStop stops before the update
// when Jira finds an error, and passes Jira's warnings on.
func TestWorkflowResourceUpdate_ValidationErrorsStop(t *testing.T) {
	mock, r, state := newUpdateMock(t)
	mock.validationErrors = []map[string]interface{}{
		{"type": "TRANSITION", "code": "TRANSITION_REFERENCES_AN_UNKNOWN_STATUS_REFERENCE", "level": "ERROR",
			"message": "Transition references an unknown status.", "elementReference": map[string]string{"transitionId": "41"}},
		{"type": "STATUS_LAYOUT", "code": "NO_INBOUND_TRANSITIONS_TO_STATUS", "level": "WARNING",
			"message": "This status cannot be reached from the start of the workflow.", "elementReference": map[string]string{"statusReference": "10200"}},
	}
	before := slices.Clone(mock.doc)

	resp := runUpdate(r, state, updatePlan(t, state, addMergedStatus(t)))
	if !resp.Diagnostics.HasError() {
		t.Fatal("Update succeeded, want an error")
	}
	errs := resp.Diagnostics.Errors()
	if len(errs) != 1 || !strings.Contains(errs[0].Summary(), "Jira rejected the update") ||
		!strings.Contains(errs[0].Detail(), "Nothing was changed in Jira") ||
		!strings.Contains(errs[0].Detail(), "transition 41: Transition references an unknown status.") {
		t.Errorf("errors = %v", errs)
	}
	warnings := resp.Diagnostics.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0].Detail(), "status 10200: This status cannot be reached") {
		t.Errorf("warnings = %v", warnings)
	}
	if len(mock.updates) != 0 || !slices.Equal(mock.doc, before) {
		t.Errorf("the workflow was updated %d times, want none", len(mock.updates))
	}
}

// TestWorkflowResourceUpdate_WarningsDoNotStop updates the workflow when Jira
// only warns.
func TestWorkflowResourceUpdate_WarningsDoNotStop(t *testing.T) {
	mock, r, state := newUpdateMock(t)
	mock.validationErrors = []map[string]interface{}{
		{"type": "STATUS_LAYOUT", "code": "NO_INBOUND_TRANSITIONS_TO_STATUS", "level": "WARNING",
			"message": "This status cannot be reached from the start of the workflow."},
	}

	resp := runUpdate(r, state, updatePlan(t, state, addMergedStatus(t)))
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}
	if len(resp.Diagnostics.Warnings()) != 1 || len(mock.updates) != 1 {
		t.Errorf("warnings = %v, updates = %d; want 1 and 1", resp.Diagnostics.Warnings(), len(mock.updates))
	}
}

// TestWorkflowResourceUpdate_RefusesStatusRemoval turns Jira's missing status
// mapping error into one that names the removed status, and updates nothing.
func TestWorkflowResourceUpdate_RefusesStatusRemoval(t *testing.T) {
	mock, r, state := newUpdateMock(t)
	mock.validationErrors = []map[string]interface{}{
		{"type": "STATUS_MAPPING", "code": "INCORRECT_PROJECT_IDS_IN_STATUS_MAPPING", "level": "ERROR",
			"message": "We couldn't complete the migration because some projects are missing from the status mappings. We expected project IDs 10124, but received ."},
	}

	plan := updatePlan(t, state, func(m *workflowResourceModel, tr map[string]workflowTransitionValue) map[string]workflowTransitionValue {
		setStatuses(t, m, "10000", "3", "10002")
		delete(tr, "21")
		done := tr["31"]
		done.From = []string{"10000", "3"}
		tr["31"] = done
		return tr
	})

	resp := runUpdate(r, state, plan)
	errs := resp.Diagnostics.Errors()
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want one", errs)
	}
	if errs[0].Summary() != "Removing a status from a workflow in use is not supported yet" ||
		!strings.Contains(errs[0].Detail(), "removes the statuses 10100 from workflow") ||
		!strings.Contains(errs[0].Detail(), "We expected project IDs 10124") {
		t.Errorf("error = %s: %s", errs[0].Summary(), errs[0].Detail())
	}
	if withPath, ok := errs[0].(interface{ Path() path.Path }); !ok || !withPath.Path().Equal(path.Root("statuses")) {
		t.Errorf("error is not on statuses: %v", errs[0])
	}
	if len(mock.updates) != 0 {
		t.Errorf("updates = %d, want 0", len(mock.updates))
	}
}

// TestWorkflowResourceUpdate_WaitsForTask waits for the task Jira starts
// before it reads the workflow back.
func TestWorkflowResourceUpdate_WaitsForTask(t *testing.T) {
	mock, r, state := newUpdateMock(t)
	mock.taskID = "10010"

	resp := runUpdate(r, state, updatePlan(t, state, addMergedStatus(t)))
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}
	i := slices.Index(mock.requests, "GET /rest/api/3/task/10010")
	if i < 0 || i != len(mock.requests)-2 {
		t.Errorf("requests = %v, want the task polled before the read back", mock.requests)
	}
}

// TestWorkflowResourceUpdate_KeepsTransitionsLeftOut sends the live
// transitions, rule IDs and ports included, when the plan has none.
func TestWorkflowResourceUpdate_KeepsTransitionsLeftOut(t *testing.T) {
	mock, r, state := newUpdateMock(t)
	plan := updatePlan(t, state, func(m *workflowResourceModel, _ map[string]workflowTransitionValue) map[string]workflowTransitionValue {
		setStatuses(t, m, "10000", "3", "10002", "10100", "10200")
		return nil
	})

	resp := runUpdate(r, state, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(mock.updates[0], &body); err != nil {
		t.Fatal(err)
	}
	var wantBody map[string]interface{}
	if err := json.Unmarshal([]byte(wantMergedUpdate), &wantBody); err != nil {
		t.Fatal(err)
	}
	got := body["workflows"].([]interface{})[0].(map[string]interface{})["transitions"].([]interface{})
	want := wantBody["workflows"].([]interface{})[0].(map[string]interface{})["transitions"].([]interface{})

	// The fixture's transitions as they are: 11 keeps its post function and its ID.
	open := want[1].(map[string]interface{})
	open["actions"] = []interface{}{map[string]interface{}{
		"id": "f9c9d922-21f8-430b-8f66-13c26c63d389", "ruleKey": "system:update-field",
		"parameters": map[string]interface{}{"field": "resolution", "value": "", "mode": ""},
	}}
	want[3].(map[string]interface{})["name"] = "Done"
	want = want[:4]
	if !reflect.DeepEqual(got, want) {
		gotPretty, _ := json.MarshalIndent(got, "", "  ")
		t.Errorf("transitions = %s", gotPretty)
	}
}

func TestWorkflowResourceUpdate_KeepsLiveTransitionsAfterPlanning(t *testing.T) {
	for _, useModifiedPlan := range []bool{false, true} {
		t.Run(map[bool]string{false: "state-derived plan", true: "modified plan"}[useModifiedPlan], func(t *testing.T) {
			mock, r, state := newUpdateMock(t)
			ctx := context.Background()
			plan := updatePlan(t, state, func(m *workflowResourceModel, tr map[string]workflowTransitionValue) map[string]workflowTransitionValue {
				m.Description = types.StringValue("Updated description")
				return tr
			})
			configPlan := updatePlan(t, state, func(m *workflowResourceModel, _ map[string]workflowTransitionValue) map[string]workflowTransitionValue {
				m.Description = types.StringValue("Updated description")
				return nil
			})
			config := tfsdk.Config{Schema: configPlan.Schema, Raw: configPlan.Raw.Copy()}
			modified := resource.ModifyPlanResponse{Plan: plan}
			r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: state, Config: config}, &modified)
			if modified.Diagnostics.HasError() {
				t.Fatalf("ModifyPlan: %v", modified.Diagnostics)
			}
			var plannedTransitions types.Map
			if diags := modified.Plan.GetAttribute(ctx, path.Root("transitions"), &plannedTransitions); diags.HasError() {
				t.Fatal(diags)
			}
			if !plannedTransitions.IsUnknown() {
				t.Fatal("unconfigured transitions must be unknown during an update")
			}
			if useModifiedPlan {
				plan = modified.Plan
			}

			var doc map[string]interface{}
			if err := json.Unmarshal(mock.doc, &doc); err != nil {
				t.Fatal(err)
			}
			wf := doc["workflows"].([]interface{})[0].(map[string]interface{})
			transitions := wf["transitions"].([]interface{})
			var changed map[string]interface{}
			for _, transition := range transitions {
				candidate := transition.(map[string]interface{})
				if candidate["id"] == "11" {
					changed = candidate
					break
				}
			}
			if changed == nil {
				t.Fatal("fixture is missing transition 11")
			}
			changed["name"] = "Live edit"
			added := make(map[string]interface{}, len(changed))
			for key, value := range changed {
				added[key] = value
			}
			added["id"], added["name"] = "41", "Live addition"
			wf["transitions"] = append(transitions, added)
			var err error
			mock.doc, err = json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}

			resp := resource.UpdateResponse{State: tfsdk.State{Schema: state.Schema, Raw: plan.Raw.Copy()}}
			r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state, Config: config}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("Update: %v", resp.Diagnostics)
			}
			var body workflowUpdateRequest
			if err := json.Unmarshal(mock.updates[0], &body); err != nil {
				t.Fatal(err)
			}
			names := map[string]string{}
			for _, tr := range body.Workflows[0].Transitions {
				names[tr.ID] = tr.Name
			}
			if names["11"] != "Live edit" || names["41"] != "Live addition" {
				t.Errorf("sent transitions = %v, want live edits", names)
			}
			var got map[string]workflowTransitionValue
			if diags := resp.State.GetAttribute(ctx, path.Root("transitions"), &got); diags.HasError() {
				t.Fatal(diags)
			}
			if got["11"].Name != "Live edit" || got["41"].Name != "Live addition" {
				t.Errorf("state transitions = %v, want live edits", got)
			}
		})
	}
}

func TestWorkflowResourceModifyPlan_ConfiguredTransitionsRemainKnown(t *testing.T) {
	_, r, state := newUpdateMock(t)
	ctx := context.Background()
	plan := updatePlan(t, state, func(m *workflowResourceModel, tr map[string]workflowTransitionValue) map[string]workflowTransitionValue {
		m.Description = types.StringValue("Updated description")
		return tr
	})
	config := tfsdk.Config{Schema: plan.Schema, Raw: plan.Raw.Copy()}
	resp := resource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: state, Config: config}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan: %v", resp.Diagnostics)
	}
	var got, want types.Map
	resp.Diagnostics.Append(resp.Plan.GetAttribute(ctx, path.Root("transitions"), &got)...)
	resp.Diagnostics.Append(plan.GetAttribute(ctx, path.Root("transitions"), &want)...)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if !got.Equal(want) {
		t.Error("configured transitions should remain in the plan")
	}
}

func TestWorkflowResourceModifyPlan_UnconfiguredTransitionsNoChange(t *testing.T) {
	_, r, state := newUpdateMock(t)
	ctx := context.Background()
	plan := updatePlan(t, state, func(_ *workflowResourceModel, tr map[string]workflowTransitionValue) map[string]workflowTransitionValue {
		return tr
	})
	configPlan := updatePlan(t, state, func(_ *workflowResourceModel, _ map[string]workflowTransitionValue) map[string]workflowTransitionValue {
		return nil
	})
	config := tfsdk.Config{Schema: configPlan.Schema, Raw: configPlan.Raw.Copy()}
	resp := resource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: state, Config: config}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan: %v", resp.Diagnostics)
	}
	var got, prior workflowResourceModel
	resp.Diagnostics.Append(resp.Plan.Get(ctx, &got)...)
	resp.Diagnostics.Append(state.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if !got.Transitions.Equal(prior.Transitions) || !got.Version.Equal(prior.Version) {
		t.Error("unchanged workflow should retain its transitions and version in the plan")
	}
}

// TestWorkflowResourceUpdate_RefusesRename fails before any request: Jira's
// update ignores a new name.
func TestWorkflowResourceUpdate_RefusesRename(t *testing.T) {
	mock, r, state := newUpdateMock(t)
	plan := updatePlan(t, state, func(m *workflowResourceModel, tr map[string]workflowTransitionValue) map[string]workflowTransitionValue {
		m.Name = types.StringValue("renamed")
		return tr
	})

	resp := runUpdate(r, state, plan)
	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "Renaming a workflow is not supported" {
		t.Errorf("diagnostics = %v, want the rename error", resp.Diagnostics)
	}
	if len(mock.requests) != 0 {
		t.Errorf("requests = %v, want none", mock.requests)
	}
}

// TestWorkflowResourceModifyPlan_RefusesRename fails the plan of a new name,
// so the change never reaches an apply.
func TestWorkflowResourceModifyPlan_RefusesRename(t *testing.T) {
	_, r, state := newUpdateMock(t)
	ctx := context.Background()

	for name, want := range map[string]bool{fixtureWorkflowName: false, "renamed": true} {
		plan := updatePlan(t, state, func(m *workflowResourceModel, tr map[string]workflowTransitionValue) map[string]workflowTransitionValue {
			m.Name = types.StringValue(name)
			return tr
		})
		resp := resource.ModifyPlanResponse{Plan: plan}
		r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: state}, &resp)
		if got := resp.Diagnostics.HasError(); got != want {
			t.Errorf("name %q: error = %v, want %v (%v)", name, got, want, resp.Diagnostics)
		}
	}
}
