package jira

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

var (
	_ resource.Resource                = &screenTabFieldsResource{}
	_ resource.ResourceWithImportState = &screenTabFieldsResource{}
)

// NewScreenTabFieldsResource returns a new resource that manages the ordered field list of a screen tab.
func NewScreenTabFieldsResource() resource.Resource {
	return &screenTabFieldsResource{}
}

type screenTabFieldsResource struct {
	client *atlassian.Client
}

type screenTabFieldsResourceModel struct {
	ID       types.String `tfsdk:"id"`
	ScreenID types.String `tfsdk:"screen_id"`
	TabID    types.String `tfsdk:"tab_id"`
	Fields   types.List   `tfsdk:"fields"`
}

// screenTabFieldMoveRequest is the body of POST .../fields/{id}/move. Jira takes
// either the field to place the moved field after, or a position such as "First".
type screenTabFieldMoveRequest struct {
	After    string `json:"after,omitempty"`
	Position string `json:"position,omitempty"`
}

// screenTabFieldMove moves one field so it follows After. An empty After moves it to the top of the tab.
type screenTabFieldMove struct {
	FieldID string
	After   string
}

func (r *screenTabFieldsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_jira_screen_tab_fields"
}

func (r *screenTabFieldsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages the complete, ordered list of fields on a tab of a Jira Cloud screen. " +
			"This resource is authoritative for the tab: on create and update it removes every field " +
			"that is not in `fields`, adds the missing ones and moves them into the listed order. " +
			"Do not use it together with `atlassian_jira_screen_tab_field` on the same tab, or the " +
			"two resources will undo each other's changes. Destroying it removes every listed field " +
			"from the tab, which also happens when `screen_id` or `tab_id` changes. To keep the fields " +
			"on a tab Terraform no longer manages, use a `removed` block with `destroy = false`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "The identifier of the resource, in the format `{screen_id}/{tab_id}`.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"screen_id": schema.StringAttribute{
				Description: "The ID of the screen. Changing this forces recreation of the resource.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"tab_id": schema.StringAttribute{
				Description: "The ID of the screen tab. Changing this forces recreation of the resource.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"fields": schema.ListAttribute{
				Description: "The IDs of the fields on the tab, in display order, for example " +
					"`summary` or `customfield_10010`. An empty list removes every field from the tab. " +
					"A field can be on only one tab of a screen.",
				Required:    true,
				ElementType: types.StringType,
				Validators: []validator.List{
					listvalidator.UniqueValues(),
					listvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1)),
				},
			},
		},
	}
}

func (r *screenTabFieldsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*atlassian.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *atlassian.Client, got: %T", req.ProviderData),
		)
		return
	}

	r.client = client
}

func (r *screenTabFieldsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan screenTabFieldsResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.apply(ctx, &plan, &resp.State, &resp.Diagnostics)
}

func (r *screenTabFieldsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state screenTabFieldsResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	screenID := state.ScreenID.ValueString()
	tabID := state.TabID.ValueString()

	current, found, err := r.readFields(ctx, screenID, tabID)
	if err != nil {
		resp.Diagnostics.AddError("Error reading screen tab fields", err.Error())
		return
	}

	if !found {
		// The screen or the tab was deleted out-of-band.
		resp.State.RemoveResource(ctx)
		return
	}

	state.ID = types.StringValue(screenTabFieldsID(screenID, tabID))
	state.Fields = screenTabFieldsList(current)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *screenTabFieldsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan screenTabFieldsResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.apply(ctx, &plan, &resp.State, &resp.Diagnostics)
}

func (r *screenTabFieldsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state screenTabFieldsResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var fields []string
	resp.Diagnostics.Append(state.Fields.ElementsAs(ctx, &fields, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	screenID := state.ScreenID.ValueString()
	tabID := state.TabID.ValueString()

	for _, fieldID := range fields {
		// A 404 means the field, the tab or the screen is already gone, so there is nothing to remove.
		if _, err := r.client.DeleteWithStatus(ctx, screenTabFieldPath(screenID, tabID, fieldID)); err != nil {
			resp.Diagnostics.AddError(
				"Error removing field from screen tab",
				fmt.Sprintf("Removing field %q from screen %s tab %s: %s", fieldID, screenID, tabID, err),
			)
			return
		}
	}
}

func (r *screenTabFieldsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import format: {screenId}/{tabId}
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.Contains(parts[1], "/") {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("Expected import ID in the format {screen_id}/{tab_id}, got %q", req.ID),
		)
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("screen_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("tab_id"), parts[1])...)
}

// apply makes the tab hold exactly the planned fields in the planned order, then
// records what Jira reports. It removes the fields that are not planned first, then
// adds the missing ones (Jira appends them), then moves the fields that are out of order.
func (r *screenTabFieldsResource) apply(ctx context.Context, plan *screenTabFieldsResourceModel, state *tfsdk.State, diags *diag.Diagnostics) {
	var desired []string
	diags.Append(plan.Fields.ElementsAs(ctx, &desired, false)...)
	if diags.HasError() {
		return
	}

	screenID := plan.ScreenID.ValueString()
	tabID := plan.TabID.ValueString()

	current, found, err := r.readFields(ctx, screenID, tabID)
	if err != nil {
		diags.AddError("Error reading screen tab fields", err.Error())
		return
	}
	if !found {
		diags.AddError(
			"Screen tab not found",
			fmt.Sprintf("Jira found no tab %s on screen %s.", tabID, screenID),
		)
		return
	}

	want := make(map[string]bool, len(desired))
	for _, fieldID := range desired {
		want[fieldID] = true
	}

	working := make([]string, 0, len(desired))
	onTab := make(map[string]bool, len(desired))
	for _, fieldID := range current {
		if want[fieldID] {
			working = append(working, fieldID)
			onTab[fieldID] = true
			continue
		}
		if _, err := r.client.DeleteWithStatus(ctx, screenTabFieldPath(screenID, tabID, fieldID)); err != nil {
			diags.AddError(
				"Error removing field from screen tab",
				fmt.Sprintf("Removing field %q from screen %s tab %s: %s", fieldID, screenID, tabID, err),
			)
			return
		}
	}

	for _, fieldID := range desired {
		if onTab[fieldID] {
			continue
		}
		if err := r.client.Post(ctx, screenTabFieldsPath(screenID, tabID), screenTabFieldWriteRequest{FieldID: fieldID}, nil); err != nil {
			diags.AddError(
				"Error adding field to screen tab",
				fmt.Sprintf("Adding field %q to screen %s tab %s: %s", fieldID, screenID, tabID, err),
			)
			return
		}
		working = append(working, fieldID)
	}

	for _, move := range planScreenTabFieldMoves(working, desired) {
		body := screenTabFieldMoveRequest{After: move.After}
		if move.After == "" {
			body = screenTabFieldMoveRequest{Position: "First"}
		}
		if err := r.client.Post(ctx, screenTabFieldPath(screenID, tabID, move.FieldID)+"/move", body, nil); err != nil {
			diags.AddError(
				"Error moving field on screen tab",
				fmt.Sprintf("Moving field %q on screen %s tab %s: %s", move.FieldID, screenID, tabID, err),
			)
			return
		}
	}

	final, found, err := r.readFields(ctx, screenID, tabID)
	if err != nil {
		diags.AddError("Error reading screen tab fields", err.Error())
		return
	}
	if !found {
		diags.AddError(
			"Screen tab not found",
			fmt.Sprintf("Jira found no tab %s on screen %s after changing its fields.", tabID, screenID),
		)
		return
	}

	plan.ID = types.StringValue(screenTabFieldsID(screenID, tabID))
	plan.Fields = screenTabFieldsList(final)
	diags.Append(state.Set(ctx, plan)...)

	if !slices.Equal(final, desired) {
		diags.AddError(
			"Screen tab fields do not match the configuration",
			fmt.Sprintf("After the changes, Jira lists the fields of screen %s tab %s as %q, but the configuration lists %q.",
				screenID, tabID, final, desired),
		)
	}
}

// readFields returns the IDs of the fields on a tab, in display order. found is
// false when Jira answers 404, because the screen or the tab does not exist.
func (r *screenTabFieldsResource) readFields(ctx context.Context, screenID, tabID string) ([]string, bool, error) {
	var fields []screenTabFieldAPIResponse
	statusCode, err := r.client.GetWithStatus(ctx, screenTabFieldsPath(screenID, tabID), &fields)
	if err != nil {
		return nil, false, err
	}
	if statusCode == http.StatusNotFound {
		return nil, false, nil
	}

	ids := make([]string, 0, len(fields))
	for _, f := range fields {
		ids = append(ids, f.ID)
	}
	return ids, true, nil
}

// planScreenTabFieldMoves returns the fewest moves that turn current into desired.
// Both lists must hold the same field IDs. The fields in a longest run that is
// already in the desired relative order stay where they are, so an unchanged prefix
// costs nothing. Every other field moves, in desired order, to just after the field
// that precedes it in desired (or to the top). Since each moved field lands right
// after a field that is already in its final place, the result is desired.
func planScreenTabFieldMoves(current, desired []string) []screenTabFieldMove {
	position := make(map[string]int, len(current))
	for i, fieldID := range current {
		position[fieldID] = i
	}

	sequence := make([]int, len(desired))
	for i, fieldID := range desired {
		sequence[i] = position[fieldID]
	}

	keep := longestIncreasingSubsequence(sequence)

	var moves []screenTabFieldMove
	for i, fieldID := range desired {
		if keep[i] {
			continue
		}
		after := ""
		if i > 0 {
			after = desired[i-1]
		}
		moves = append(moves, screenTabFieldMove{FieldID: fieldID, After: after})
	}
	return moves
}

// longestIncreasingSubsequence marks the indexes of one longest strictly increasing
// subsequence of values (patience sorting, O(n log n)).
func longestIncreasingSubsequence(values []int) []bool {
	// tails[k] is the index of the smallest value that ends an increasing run of length k+1.
	tails := make([]int, 0, len(values))
	previous := make([]int, len(values))

	for i, v := range values {
		k, _ := slices.BinarySearchFunc(tails, v, func(tail, target int) int {
			return values[tail] - target
		})
		if k > 0 {
			previous[i] = tails[k-1]
		} else {
			previous[i] = -1
		}
		if k == len(tails) {
			tails = append(tails, i)
		} else {
			tails[k] = i
		}
	}

	keep := make([]bool, len(values))
	if len(tails) == 0 {
		return keep
	}
	for i := tails[len(tails)-1]; i >= 0; i = previous[i] {
		keep[i] = true
	}
	return keep
}

func screenTabFieldsID(screenID, tabID string) string {
	return screenID + "/" + tabID
}

func screenTabFieldsPath(screenID, tabID string) string {
	return fmt.Sprintf("/rest/api/3/screens/%s/tabs/%s/fields",
		atlassian.PathEscape(screenID),
		atlassian.PathEscape(tabID),
	)
}

func screenTabFieldPath(screenID, tabID, fieldID string) string {
	return screenTabFieldsPath(screenID, tabID) + "/" + atlassian.PathEscape(fieldID)
}

func screenTabFieldsList(fieldIDs []string) types.List {
	values := make([]attr.Value, 0, len(fieldIDs))
	for _, fieldID := range fieldIDs {
		values = append(values, types.StringValue(fieldID))
	}
	return types.ListValueMust(types.StringType, values)
}
