package jira

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

var (
	_ resource.Resource                = &workflowResource{}
	_ resource.ResourceWithImportState = &workflowResource{}
	_ resource.ResourceWithModifyPlan  = &workflowResource{}
)

// NewWorkflowResource returns a new workflow resource.
func NewWorkflowResource() resource.Resource {
	return &workflowResource{}
}

type workflowResource struct {
	client *atlassian.Client
}

type workflowResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Statuses    types.List   `tfsdk:"statuses"`
	Transitions types.Map    `tfsdk:"transitions"`
	Version     types.Object `tfsdk:"version"`
}

// API request/response types

// workflowCreateRequest represents the POST /rest/api/3/workflows/create request body.
// Jira removed the legacy POST /rest/api/3/workflow endpoint (it now answers 405).
type workflowCreateRequest struct {
	Scope     workflowScope          `json:"scope"`
	Statuses  []workflowCreateStatus `json:"statuses"`
	Workflows []workflowCreateItem   `json:"workflows"`
}

type workflowScope struct {
	Type string `json:"type"`
}

// workflowCreateStatus reuses an existing status. Jira requires name and
// statusCategory even then, so they are sent as they are now, unchanged.
type workflowCreateStatus struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	StatusCategory  string `json:"statusCategory"`
	StatusReference string `json:"statusReference"`
}

type workflowCreateItem struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description,omitempty"`
	Statuses    []workflowCreateStatusRef  `json:"statuses"`
	Transitions []workflowCreateTransition `json:"transitions"`
}

type workflowCreateStatusRef struct {
	StatusReference string            `json:"statusReference"`
	Properties      map[string]string `json:"properties"`
}

type workflowCreateTransition struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Type              string            `json:"type"`
	ToStatusReference string            `json:"toStatusReference"`
	Links             []interface{}     `json:"links"`
	Actions           []interface{}     `json:"actions"`
	Validators        []interface{}     `json:"validators"`
	Triggers          []interface{}     `json:"triggers"`
	Properties        map[string]string `json:"properties"`
}

// workflowCreateResponse represents the POST /rest/api/3/workflows/create response.
type workflowCreateResponse struct {
	Workflows []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"workflows"`
}

type workflowAPIItem struct {
	ID struct {
		Name     string `json:"name"`
		EntityID string `json:"entityId"`
	} `json:"id"`
	Description string `json:"description"`
	Statuses    []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"statuses"`
}

type workflowSearchResponse struct {
	Values []workflowAPIItem `json:"values"`
}

func (r *workflowResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_jira_workflow"
}

func (r *workflowResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a Jira Cloud workflow: its name, description, statuses and transitions. " +
			"This version reads and imports a workflow with its transitions and shows any drift in a plan. " +
			"It cannot change an existing workflow yet: a change is planned as an in-place update, never a " +
			"replacement, and the apply fails before it calls Jira. A new workflow is created with its " +
			"statuses and an initial transition into the first status; `transitions` must be left out of a " +
			"new workflow's configuration.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "The entity ID (UUID) of the workflow.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Description: "The name of the workflow.",
				Required:    true,
			},
			"description": schema.StringAttribute{
				Description: "The description of the workflow.",
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(""),
			},
			"statuses": schema.ListAttribute{
				Description: "The IDs of the statuses the workflow uses. A new workflow's initial transition " +
					"goes to the first. When the workflow is read, the order in state is kept if Jira holds " +
					"the same statuses; an import takes Jira's order.",
				Required:    true,
				ElementType: types.StringType,
			},
			"transitions": workflowTransitionsAttribute(),
			"version": schema.SingleNestedAttribute{
				Description: "The workflow's current version. Jira needs it to update the workflow.",
				Computed:    true,
				Attributes: map[string]schema.Attribute{
					"id": schema.StringAttribute{
						Description: "The version's ID.",
						Computed:    true,
					},
					"number": schema.Int64Attribute{
						Description: "The version number. Jira raises it on every change.",
						Computed:    true,
					},
				},
			},
		},
	}
}

func (r *workflowResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *workflowResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan workflowResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !plan.Transitions.IsNull() && !plan.Transitions.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("transitions"),
			"Creating a workflow with transitions is not supported yet",
			"This version of the provider creates a workflow with its statuses and an initial transition "+
				"only. Leave transitions out of the configuration to create the workflow, or import an "+
				"existing workflow.")
		return
	}

	// Extract status IDs from the plan list
	var statusIDs []string
	resp.Diagnostics.Append(plan.Statuses.ElementsAs(ctx, &statusIDs, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if len(statusIDs) == 0 {
		resp.Diagnostics.AddError("Invalid workflow configuration", "At least one status is required")
		return
	}

	statuses, err := r.getStatuses(ctx, statusIDs)
	if err != nil {
		resp.Diagnostics.AddError("Error reading workflow statuses", err.Error())
		return
	}

	body := newWorkflowCreateRequest(plan.Name.ValueString(), plan.Description.ValueString(), statuses)

	var result workflowCreateResponse
	err = r.client.Post(ctx, "/rest/api/3/workflows/create", body, &result)
	if err != nil {
		resp.Diagnostics.AddError("Error creating workflow", err.Error())
		return
	}

	if len(result.Workflows) != 1 || result.Workflows[0].ID == "" {
		resp.Diagnostics.AddError("Error creating workflow",
			fmt.Sprintf("expected one created workflow with an ID, got %d", len(result.Workflows)))
		return
	}

	plan.ID = types.StringValue(result.Workflows[0].ID)

	// Read the workflow back for the values Jira sets: its transitions and version.
	found, statusRefs, err := r.fetchWorkflow(ctx, workflowReadRequest{WorkflowIDs: []string{result.Workflows[0].ID}})
	if err != nil {
		resp.Diagnostics.AddError("Error reading created workflow", err.Error())
		return
	}
	if found == nil {
		resp.Diagnostics.AddError("Error reading created workflow",
			fmt.Sprintf("workflow %s was created but cannot be read back", result.Workflows[0].ID))
		return
	}

	resp.Diagnostics.Append(applyWorkflowRead(ctx, &plan, found, statusRefs)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// statusLookupBatch is the most IDs GET /rest/api/3/statuses accepts in one call.
const statusLookupBatch = 50

// getStatuses fetches the given statuses in order. The create call needs each
// status's name and category, not only its ID.
func (r *workflowResource) getStatuses(ctx context.Context, ids []string) ([]statusAPIItem, error) {
	byID := make(map[string]statusAPIItem, len(ids))
	for batch := range slices.Chunk(ids, statusLookupBatch) {
		query := make([]string, len(batch))
		for i, id := range batch {
			query[i] = "id=" + atlassian.QueryEscape(id)
		}

		var found []statusAPIItem
		if err := r.client.Get(ctx, "/rest/api/3/statuses?"+strings.Join(query, "&"), &found); err != nil {
			return nil, err
		}
		for _, s := range found {
			byID[s.ID] = s
		}
	}

	statuses := make([]statusAPIItem, len(ids))
	for i, id := range ids {
		s, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("status %q not found", id)
		}
		statuses[i] = s
	}
	return statuses, nil
}

// newWorkflowCreateRequest builds a global workflow that reuses existing
// statuses. Each status's ID is its statusReference, which Jira accepts for
// company-managed workflows. Like before, the only transition is the initial
// one into the first status.
func newWorkflowCreateRequest(name, description string, statuses []statusAPIItem) workflowCreateRequest {
	createStatuses := make([]workflowCreateStatus, len(statuses))
	statusRefs := make([]workflowCreateStatusRef, len(statuses))
	for i, s := range statuses {
		createStatuses[i] = workflowCreateStatus{
			ID:              s.ID,
			Name:            s.Name,
			Description:     s.Description,
			StatusCategory:  s.statusCategoryKey(),
			StatusReference: s.ID,
		}
		statusRefs[i] = workflowCreateStatusRef{
			StatusReference: s.ID,
			Properties:      map[string]string{},
		}
	}

	return workflowCreateRequest{
		Scope:    workflowScope{Type: "GLOBAL"},
		Statuses: createStatuses,
		Workflows: []workflowCreateItem{{
			Name:        name,
			Description: description,
			Statuses:    statusRefs,
			Transitions: []workflowCreateTransition{{
				ID:                "1",
				Name:              "Create",
				Type:              "INITIAL",
				ToStatusReference: statuses[0].ID,
				Links:             []interface{}{},
				Actions:           []interface{}{},
				Validators:        []interface{}{},
				Triggers:          []interface{}{},
				Properties:        map[string]string{},
			}},
		}},
	}
}

func (r *workflowResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state workflowResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	found, statusIDs, err := r.fetchWorkflow(ctx, workflowReadRequest{WorkflowIDs: []string{state.ID.ValueString()}})
	if err != nil {
		resp.Diagnostics.AddError("Error reading workflow", err.Error())
		return
	}

	if found == nil {
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(applyWorkflowRead(ctx, &state, found, statusIDs)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// fetchWorkflow reads one workflow through the bulk read, POST /rest/api/3/workflows.
// It returns nil when no workflow matches; Jira answers 404 then. It also
// returns the map from status reference to status ID.
func (r *workflowResource) fetchWorkflow(ctx context.Context, body workflowReadRequest) (*workflowReadItem, map[string]string, error) {
	var result workflowReadResponse
	status, err := r.client.PostWithStatus(ctx, "/rest/api/3/workflows", body, &result)
	if err != nil {
		return nil, nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil, nil
	}

	for i := range result.Workflows {
		wf := &result.Workflows[i]
		if slices.Contains(body.WorkflowIDs, wf.ID) || slices.Contains(body.WorkflowNames, wf.Name) {
			return wf, workflowStatusIDs(result.Statuses), nil
		}
	}
	return nil, nil, nil
}

// applyWorkflowRead copies a read workflow into the model. It keeps the
// model's status order when Jira holds the same statuses, so a reordering on
// Jira's side does not show up as a change.
func applyWorkflowRead(ctx context.Context, model *workflowResourceModel, found *workflowReadItem, statusIDs map[string]string) diag.Diagnostics {
	var diags diag.Diagnostics

	transitions, err := workflowTransitionsFromAPI(*found, statusIDs)
	if err != nil {
		diags.AddError("Error reading workflow transitions", err.Error())
		return diags
	}
	transitionsValue, err := workflowTransitionsMapValue(ctx, transitions)
	if err != nil {
		diags.AddError("Error reading workflow transitions", err.Error())
		return diags
	}

	statuses := workflowStatusesFromAPI(*found, statusIDs)
	if !model.Statuses.IsNull() && !model.Statuses.IsUnknown() {
		var prior []string
		diags.Append(model.Statuses.ElementsAs(ctx, &prior, false)...)
		if diags.HasError() {
			return diags
		}
		if sameStatusSet(prior, statuses) {
			statuses = prior
		}
	}
	statusList, d := types.ListValueFrom(ctx, types.StringType, statuses)
	diags.Append(d...)

	version, d := types.ObjectValueFrom(ctx, workflowVersionAttrTypes, workflowVersionValue{
		ID:     found.Version.ID,
		Number: found.Version.VersionNumber,
	})
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}

	model.ID = types.StringValue(found.ID)
	model.Name = types.StringValue(found.Name)
	model.Description = types.StringValue(found.Description)
	model.Statuses = statusList
	model.Transitions = transitionsValue
	model.Version = version
	return diags
}

// ModifyPlan keeps the prior version when nothing else changes.
//
// When the configuration leaves out transitions, Terraform proposes null for
// them, because their nested attributes are not all computed. The
// transitions' UseStateForUnknown restores them, but the framework has by then
// marked the computed version unknown, which would plan an update with no
// change. The version only changes when the workflow does, so it stays known
// unless another attribute is planned to change.
func (r *workflowResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}

	var plan, state workflowResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if plan.ID.Equal(state.ID) && plan.Name.Equal(state.Name) && plan.Description.Equal(state.Description) &&
		plan.Statuses.Equal(state.Statuses) && plan.Transitions.Equal(state.Transitions) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("version"), state.Version)...)
	}
}

// Update refuses every change. It makes no API call, so a failed apply leaves
// the workflow as it was. Replacing the workflow instead would delete a live
// workflow, so no attribute forces a replacement.
func (r *workflowResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError(
		"Updating a workflow is not supported yet",
		"This version of the provider reads a workflow, transitions included, but cannot change it. "+
			"Nothing was changed in Jira. Revert the change in the configuration, or make it in Jira.",
	)
}

func (r *workflowResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state workflowResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiPath := fmt.Sprintf("/rest/api/3/workflow/%s", atlassian.PathEscape(state.ID.ValueString()))
	statusCode, err := r.client.DeleteWithStatus(ctx, apiPath)
	if err != nil {
		resp.Diagnostics.AddError("Error deleting workflow", err.Error())
		return
	}

	// 404 means the workflow was already deleted out-of-band; treat as success.
	if statusCode == http.StatusNotFound {
		return
	}
}

// ImportState takes a workflow's entity ID or its name. It looks the ID up
// first, then the name, and stores the entity ID; Read fills in the rest.
func (r *workflowResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	found, _, err := r.fetchWorkflow(ctx, workflowReadRequest{WorkflowIDs: []string{req.ID}})
	if err == nil && found == nil {
		found, _, err = r.fetchWorkflow(ctx, workflowReadRequest{WorkflowNames: []string{req.ID}})
	}
	if err != nil {
		resp.Diagnostics.AddError("Error importing workflow", err.Error())
		return
	}
	if found == nil {
		resp.Diagnostics.AddError("Error importing workflow",
			fmt.Sprintf("no workflow has the ID or name %q", req.ID))
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), found.ID)...)
}
