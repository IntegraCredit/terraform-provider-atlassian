package jira

import (
	"context"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

// These tests call the screen tab fields resource directly against FakeScreenTabs.
// They need no Terraform CLI and no TF_ACC, so they run in `make test` and CI.

const (
	tabFieldsScreenID = "10100"
	tabFieldsTabID    = "10200"
	tabFieldsPrefix   = "/rest/api/3/screens/10100/tabs/10200/fields"
)

func newTabFieldsResource(t *testing.T, fake *FakeScreenTabs) *screenTabFieldsResource {
	t.Helper()
	client, err := atlassian.NewClient(atlassian.ClientConfig{
		URL:   fake.URL(),
		User:  "test@test.com",
		Token: "test-token",
	})
	if err != nil {
		t.Fatalf("creating client: %s", err)
	}
	return &screenTabFieldsResource{client: client}
}

func tabFieldsSchema(t *testing.T, r *screenTabFieldsResource) resource.SchemaResponse {
	t.Helper()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", resp.Diagnostics)
	}
	return resp
}

func tabFieldsModel(fields ...string) *screenTabFieldsResourceModel {
	return &screenTabFieldsResourceModel{
		ID:       types.StringValue(tabFieldsScreenID + "/" + tabFieldsTabID),
		ScreenID: types.StringValue(tabFieldsScreenID),
		TabID:    types.StringValue(tabFieldsTabID),
		Fields:   screenTabFieldsList(fields),
	}
}

func tabFieldsState(t *testing.T, r *screenTabFieldsResource, model *screenTabFieldsResourceModel) tfsdk.State {
	t.Helper()
	ctx := context.Background()
	s := tabFieldsSchema(t, r).Schema
	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if model != nil {
		if diags := state.Set(ctx, model); diags.HasError() {
			t.Fatalf("building state: %v", diags)
		}
	}
	return state
}

func tabFieldsPlan(t *testing.T, r *screenTabFieldsResource, fields ...string) tfsdk.Plan {
	t.Helper()
	state := tabFieldsState(t, r, tabFieldsModel(fields...))
	return tfsdk.Plan{Schema: state.Schema, Raw: state.Raw}
}

func stateFields(t *testing.T, state tfsdk.State) []string {
	t.Helper()
	var model screenTabFieldsResourceModel
	if diags := state.Get(context.Background(), &model); diags.HasError() {
		t.Fatalf("reading state: %v", diags)
	}
	var fields []string
	if diags := model.Fields.ElementsAs(context.Background(), &fields, false); diags.HasError() {
		t.Fatalf("reading fields: %v", diags)
	}
	return fields
}

func updateTabFields(t *testing.T, r *screenTabFieldsResource, prior []string, fields ...string) resource.UpdateResponse {
	t.Helper()
	plan := tabFieldsPlan(t, r, fields...)
	resp := resource.UpdateResponse{State: tfsdk.State{Schema: plan.Schema, Raw: plan.Raw.Copy()}}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  plan,
		State: tabFieldsState(t, r, tabFieldsModel(prior...)),
	}, &resp)
	return resp
}

// TestScreenTabFieldsUpdate_Reconciles removes, adds and reorders in one apply.
func TestScreenTabFieldsUpdate_Reconciles(t *testing.T) {
	fake := NewFakeScreenTabs(t)
	fake.SetFields(tabFieldsScreenID, tabFieldsTabID, "summary", "issuetype", "description", "priority", "labels")
	r := newTabFieldsResource(t, fake)

	resp := updateTabFields(t, r, []string{"summary", "issuetype", "description", "priority", "labels"},
		"summary", "issuetype", "priority", "description", "customfield_10010")
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}

	want := []string{"summary", "issuetype", "priority", "description", "customfield_10010"}
	if got := fake.Fields(tabFieldsScreenID, tabFieldsTabID); !slices.Equal(got, want) {
		t.Errorf("tab fields = %q, want %q", got, want)
	}
	if got := stateFields(t, resp.State); !slices.Equal(got, want) {
		t.Errorf("state fields = %q, want %q", got, want)
	}

	// labels goes, customfield_10010 is appended, and one move puts priority before description.
	wantWrites := []string{
		"DELETE " + tabFieldsPrefix + "/labels",
		"POST " + tabFieldsPrefix + " customfield_10010",
		"POST " + tabFieldsPrefix + "/priority/move map[after:issuetype]",
	}
	if got := fake.Writes(); !slices.Equal(got, wantWrites) {
		t.Errorf("writes =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(wantWrites, "\n  "))
	}
}

// TestScreenTabFieldsUpdate_UnchangedMakesNoWrites checks that a tab already in order costs only reads.
func TestScreenTabFieldsUpdate_UnchangedMakesNoWrites(t *testing.T) {
	fake := NewFakeScreenTabs(t)
	fake.SetFields(tabFieldsScreenID, tabFieldsTabID, "summary", "description", "labels")
	r := newTabFieldsResource(t, fake)

	resp := updateTabFields(t, r, []string{"summary"}, "summary", "description", "labels")
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}
	if got := fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %q, want none", got)
	}
}

// TestScreenTabFieldsUpdate_MovesToTop uses position First for the field that leads the list.
func TestScreenTabFieldsUpdate_MovesToTop(t *testing.T) {
	fake := NewFakeScreenTabs(t)
	fake.SetFields(tabFieldsScreenID, tabFieldsTabID, "summary", "description", "labels")
	r := newTabFieldsResource(t, fake)

	resp := updateTabFields(t, r, nil, "labels", "summary", "description")
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}
	wantWrites := []string{"POST " + tabFieldsPrefix + "/labels/move map[position:First]"}
	if got := fake.Writes(); !slices.Equal(got, wantWrites) {
		t.Errorf("writes = %q, want %q", got, wantWrites)
	}
}

// TestScreenTabFieldsUpdate_EmptyList removes every field from the tab.
func TestScreenTabFieldsUpdate_EmptyList(t *testing.T) {
	fake := NewFakeScreenTabs(t)
	fake.SetFields(tabFieldsScreenID, tabFieldsTabID, "summary", "description")
	r := newTabFieldsResource(t, fake)

	resp := updateTabFields(t, r, []string{"summary", "description"})
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics)
	}
	if got := fake.Fields(tabFieldsScreenID, tabFieldsTabID); len(got) != 0 {
		t.Errorf("tab fields = %q, want none", got)
	}
	if got := stateFields(t, resp.State); len(got) != 0 {
		t.Errorf("state fields = %q, want none", got)
	}
}

// TestScreenTabFieldsUpdate_ReportsOrderJiraDidNotKeep fails the apply when the read-back order is wrong.
func TestScreenTabFieldsUpdate_ReportsOrderJiraDidNotKeep(t *testing.T) {
	fake := NewFakeScreenTabs(t)
	fake.SetFields(tabFieldsScreenID, tabFieldsTabID, "summary", "description")
	fake.IgnoreMoves()
	r := newTabFieldsResource(t, fake)

	resp := updateTabFields(t, r, nil, "description", "summary")
	if !resp.Diagnostics.HasError() {
		t.Fatal("Update succeeded, want an error about the order")
	}
	if got := resp.Diagnostics.Errors()[0].Summary(); got != "Screen tab fields do not match the configuration" {
		t.Errorf("error summary = %q", got)
	}
	// State records what Jira has, so the next plan shows the difference.
	if got, want := stateFields(t, resp.State), []string{"summary", "description"}; !slices.Equal(got, want) {
		t.Errorf("state fields = %q, want %q", got, want)
	}
}

// TestScreenTabFieldsCreate_TakesOverTab removes fields the configuration does not list.
func TestScreenTabFieldsCreate_TakesOverTab(t *testing.T) {
	fake := NewFakeScreenTabs(t)
	fake.SetFields(tabFieldsScreenID, tabFieldsTabID, "labels", "summary")
	r := newTabFieldsResource(t, fake)

	plan := tabFieldsPlan(t, r, "summary", "description")
	resp := resource.CreateResponse{State: tabFieldsState(t, r, nil)}
	r.Create(context.Background(), resource.CreateRequest{Plan: plan}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}

	want := []string{"summary", "description"}
	if got := fake.Fields(tabFieldsScreenID, tabFieldsTabID); !slices.Equal(got, want) {
		t.Errorf("tab fields = %q, want %q", got, want)
	}
	var model screenTabFieldsResourceModel
	resp.State.Get(context.Background(), &model)
	if got := model.ID.ValueString(); got != "10100/10200" {
		t.Errorf("id = %q, want 10100/10200", got)
	}
}

// TestScreenTabFieldsCreate_OrderJiraDidNotKeepSavesNoState fails without saving state.
// Saved state would taint the resource, and the next apply would empty the tab before
// adding the fields back. Without state, the next apply runs Create again.
func TestScreenTabFieldsCreate_OrderJiraDidNotKeepSavesNoState(t *testing.T) {
	fake := NewFakeScreenTabs(t)
	fake.SetFields(tabFieldsScreenID, tabFieldsTabID, "summary", "description")
	fake.IgnoreMoves()
	r := newTabFieldsResource(t, fake)

	plan := tabFieldsPlan(t, r, "description", "summary")
	resp := resource.CreateResponse{State: tabFieldsState(t, r, nil)}
	r.Create(context.Background(), resource.CreateRequest{Plan: plan}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("Create succeeded, want an error about the order")
	}
	if got := resp.Diagnostics.Errors()[0].Summary(); got != "Screen tab fields do not match the configuration" {
		t.Errorf("error summary = %q", got)
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state = %s, want null", resp.State.Raw)
	}
}

// TestScreenTabFieldsCreate_MissingTab fails before any write.
func TestScreenTabFieldsCreate_MissingTab(t *testing.T) {
	fake := NewFakeScreenTabs(t)
	r := newTabFieldsResource(t, fake)

	plan := tabFieldsPlan(t, r, "summary")
	resp := resource.CreateResponse{State: tabFieldsState(t, r, nil)}
	r.Create(context.Background(), resource.CreateRequest{Plan: plan}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("Create succeeded on a tab that does not exist")
	}
	if got := resp.Diagnostics.Errors()[0].Summary(); got != "Screen tab not found" {
		t.Errorf("error summary = %q", got)
	}
}

// TestScreenTabFieldsRead_RecordsOrder reads the order from Jira, which is how a reorder in the UI shows as drift.
func TestScreenTabFieldsRead_RecordsOrder(t *testing.T) {
	fake := NewFakeScreenTabs(t)
	fake.SetFields(tabFieldsScreenID, tabFieldsTabID, "description", "summary", "labels")
	r := newTabFieldsResource(t, fake)

	state := tabFieldsState(t, r, tabFieldsModel("summary", "description"))
	resp := resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}
	if got, want := stateFields(t, resp.State), []string{"description", "summary", "labels"}; !slices.Equal(got, want) {
		t.Errorf("state fields = %q, want %q", got, want)
	}
}

// TestScreenTabFieldsRead_TabGone drops the resource when Jira answers 404.
func TestScreenTabFieldsRead_TabGone(t *testing.T) {
	fake := NewFakeScreenTabs(t)
	r := newTabFieldsResource(t, fake)

	state := tabFieldsState(t, r, tabFieldsModel("summary"))
	resp := resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Error("state was kept for a tab that no longer exists")
	}
}

// TestScreenTabFieldsDelete_RemovesListedFields removes the fields in state and tolerates ones already gone.
func TestScreenTabFieldsDelete_RemovesListedFields(t *testing.T) {
	fake := NewFakeScreenTabs(t)
	fake.SetFields(tabFieldsScreenID, tabFieldsTabID, "summary", "labels")
	r := newTabFieldsResource(t, fake)

	state := tabFieldsState(t, r, tabFieldsModel("summary", "description"))
	resp := resource.DeleteResponse{State: state}
	r.Delete(context.Background(), resource.DeleteRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Delete: %v", resp.Diagnostics)
	}
	// description was already gone (404). labels is not in state, so it stays.
	if got, want := fake.Fields(tabFieldsScreenID, tabFieldsTabID), []string{"labels"}; !slices.Equal(got, want) {
		t.Errorf("tab fields = %q, want %q", got, want)
	}
}

// TestScreenTabFieldsDelete_TabGone treats a missing tab as already empty.
func TestScreenTabFieldsDelete_TabGone(t *testing.T) {
	fake := NewFakeScreenTabs(t)
	r := newTabFieldsResource(t, fake)

	state := tabFieldsState(t, r, tabFieldsModel("summary"))
	resp := resource.DeleteResponse{State: state}
	r.Delete(context.Background(), resource.DeleteRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Delete: %v", resp.Diagnostics)
	}
}

func TestScreenTabFieldsImportState(t *testing.T) {
	ctx := context.Background()
	r := &screenTabFieldsResource{}

	resp := resource.ImportStateResponse{State: tabFieldsState(t, r, nil)}
	r.ImportState(ctx, resource.ImportStateRequest{ID: "10100/10200"}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ImportState: %v", resp.Diagnostics)
	}
	var model screenTabFieldsResourceModel
	resp.State.Get(ctx, &model)
	if model.ID.ValueString() != "10100/10200" || model.ScreenID.ValueString() != "10100" || model.TabID.ValueString() != "10200" {
		t.Errorf("imported id=%q screen_id=%q tab_id=%q", model.ID.ValueString(), model.ScreenID.ValueString(), model.TabID.ValueString())
	}

	for _, id := range []string{"", "10100", "10100/", "/10200", "10100/10200/summary"} {
		resp := resource.ImportStateResponse{State: tabFieldsState(t, r, nil)}
		r.ImportState(ctx, resource.ImportStateRequest{ID: id}, &resp)
		if !resp.Diagnostics.HasError() {
			t.Errorf("ImportState(%q) succeeded, want an error", id)
		}
	}
}

// applyMoves replays moves the way Jira applies them.
func applyMoves(t *testing.T, fields []string, moves []screenTabFieldMove) []string {
	t.Helper()
	out := slices.Clone(fields)
	for _, m := range moves {
		i := slices.Index(out, m.FieldID)
		if i < 0 {
			t.Fatalf("move of %q, which is not on the tab", m.FieldID)
		}
		out = slices.Delete(out, i, i+1)
		j := 0
		if m.After != "" {
			j = slices.Index(out, m.After) + 1
			if j == 0 {
				t.Fatalf("move of %q after %q, which is not on the tab", m.FieldID, m.After)
			}
		}
		out = slices.Insert(out, j, m.FieldID)
	}
	return out
}

// lisLength is a plain O(n²) longest increasing subsequence length, to check the fast one.
func lisLength(values []int) int {
	best := 0
	length := make([]int, len(values))
	for i := range values {
		length[i] = 1
		for j := 0; j < i; j++ {
			if values[j] < values[i] && length[j]+1 > length[i] {
				length[i] = length[j] + 1
			}
		}
		best = max(best, length[i])
	}
	return best
}

func TestPlanScreenTabFieldMoves(t *testing.T) {
	cases := []struct {
		name    string
		current []string
		desired []string
		moves   int
	}{
		{"empty", nil, nil, 0},
		{"same order", []string{"a", "b", "c"}, []string{"a", "b", "c"}, 0},
		{"swap tail", []string{"a", "b", "c", "d"}, []string{"a", "b", "d", "c"}, 1},
		{"last to top", []string{"a", "b", "c", "d"}, []string{"d", "a", "b", "c"}, 1},
		{"top to last", []string{"a", "b", "c", "d"}, []string{"b", "c", "d", "a"}, 1},
		{"reversed", []string{"a", "b", "c", "d"}, []string{"d", "c", "b", "a"}, 3},
		{"interleaved", []string{"a", "b", "c", "d", "e", "f"}, []string{"b", "a", "d", "c", "f", "e"}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			moves := planScreenTabFieldMoves(tc.current, tc.desired)
			if len(moves) != tc.moves {
				t.Errorf("%d moves %v, want %d", len(moves), moves, tc.moves)
			}
			if got := applyMoves(t, tc.current, moves); !slices.Equal(got, tc.desired) {
				t.Errorf("after moves %q, want %q", got, tc.desired)
			}
		})
	}
}

// TestPlanScreenTabFieldMoves_Random checks many permutations: the moves give the
// desired order, their number is the minimum (n minus the longest kept run), and a
// prefix already in place is never moved.
func TestPlanScreenTabFieldMoves_Random(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 2000; iter++ {
		n := rng.Intn(40)
		current := make([]string, n)
		for i := range current {
			current[i] = "customfield_" + string(rune('A'+i/26)) + string(rune('a'+i%26))
		}
		desired := slices.Clone(current)
		rng.Shuffle(n, func(i, j int) { desired[i], desired[j] = desired[j], desired[i] })
		if n > 0 && rng.Intn(2) == 0 {
			// Keep a random prefix in place, as most edits do.
			p := rng.Intn(n)
			desired = append(slices.Clone(current[:p]), shuffledWithout(rng, current, current[:p])...)
		}

		moves := planScreenTabFieldMoves(current, desired)
		if got := applyMoves(t, current, moves); !slices.Equal(got, desired) {
			t.Fatalf("current %q desired %q: after moves %q", current, desired, got)
		}

		position := map[string]int{}
		for i, f := range current {
			position[f] = i
		}
		sequence := make([]int, n)
		for i, f := range desired {
			sequence[i] = position[f]
		}
		if want := n - lisLength(sequence); len(moves) != want {
			t.Fatalf("current %q desired %q: %d moves, want %d", current, desired, len(moves), want)
		}

		prefix := 0
		for prefix < n && current[prefix] == desired[prefix] {
			prefix++
		}
		for _, m := range moves {
			if slices.Index(desired, m.FieldID) < prefix {
				t.Fatalf("current %q desired %q: moved %q from the unchanged prefix", current, desired, m.FieldID)
			}
		}
	}
}

func shuffledWithout(rng *rand.Rand, all, drop []string) []string {
	var rest []string
	for _, f := range all {
		if !slices.Contains(drop, f) {
			rest = append(rest, f)
		}
	}
	rng.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	return rest
}
