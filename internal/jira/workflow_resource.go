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
			"A change to an existing workflow is an in-place update, never a replacement. The provider " +
			"sends the whole workflow to Jira's bulk update API once Jira has validated it, and keeps what " +
			"it does not manage as Jira holds it: the diagram layout, status properties, transition screens " +
			"and the IDs of rules that stay. Jira applies the update to the live workflow, even when " +
			"projects use it, with no draft workflow scheme. Two changes are refused before Jira changes " +
			"anything: a new " +
			"`name`, because Jira's API cannot rename a workflow, and removing a status from a workflow " +
			"that projects use, because Jira then needs status mappings, which this version does not send. " +
			"A new workflow is created with its statuses and an initial transition into the first status; " +
			"`transitions` must be left out of a new workflow's configuration.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "The entity ID (UUID) of the workflow.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Description: "The name of the workflow. Jira's API cannot rename a workflow, so a plan " +
					"that changes it fails; rename the workflow in Jira, then change this to match.",
				Required: true,
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
					"the same statuses; an import takes Jira's order. A status can be added to a workflow in " +
					"use; removing one from a workflow that projects use fails before Jira changes anything.",
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

// ModifyPlan refuses a new name, which Jira's API cannot set, and keeps the
// prior version when nothing else changes.
//
// When the configuration leaves out transitions, Terraform proposes null for
// them, because their nested attributes are not all computed. The
// transitions' UseStateForUnknown restores them, but the framework has by then
// marked the computed version unknown, which would plan an update with no
// change. The version only changes when the workflow does, so it stays known
// unless another attribute is planned to change. On an update, unconfigured
// transitions must remain unknown so the live values can be saved in state.
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

	if !plan.Name.IsUnknown() && !plan.Name.Equal(state.Name) {
		resp.Diagnostics.Append(workflowRenameError(state.Name.ValueString(), plan.Name.ValueString()))
		return
	}

	if plan.ID.Equal(state.ID) && plan.Name.Equal(state.Name) && plan.Description.Equal(state.Description) &&
		plan.Statuses.Equal(state.Statuses) && plan.Transitions.Equal(state.Transitions) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("version"), state.Version)...)
		return
	}

	var configuredTransitions types.Map
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("transitions"), &configuredTransitions)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if configuredTransitions.IsNull() {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("transitions"),
			types.MapUnknown(workflowTransitionObjectType()))...)
	}
}

// workflowRenameError explains that a workflow cannot be renamed. Jira's bulk
// update takes no name and ignores one, and no other API renames a workflow.
func workflowRenameError(from, to string) diag.Diagnostic {
	return diag.NewAttributeErrorDiagnostic(path.Root("name"),
		"Renaming a workflow is not supported",
		fmt.Sprintf("The configuration renames workflow %q to %q, but Jira's API cannot rename a workflow. "+
			"Nothing was changed in Jira. Rename the workflow in Jira's workflow editor, then change the "+
			"name here to match.", from, to))
}

// Update gives the workflow the planned description, statuses and
// transitions through POST /rest/api/3/workflows/update. It reads the live
// workflow first, for its version and for what the provider does not manage,
// and has Jira validate the request before sending it, so a refused change
// leaves the workflow as it was.
func (r *workflowResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state workflowResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !plan.Name.Equal(state.Name) {
		resp.Diagnostics.Append(workflowRenameError(state.Name.ValueString(), plan.Name.ValueString()))
		return
	}

	var statusIDs []string
	resp.Diagnostics.Append(plan.Statuses.ElementsAs(ctx, &statusIDs, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(statusIDs) == 0 {
		resp.Diagnostics.AddError("Invalid workflow configuration", "At least one status is required")
		return
	}

	live, liveIDs, err := r.fetchWorkflow(ctx, workflowReadRequest{WorkflowIDs: []string{state.ID.ValueString()}})
	if err != nil {
		resp.Diagnostics.AddError("Error reading workflow before the update", err.Error())
		return
	}
	if live == nil {
		resp.Diagnostics.AddError("Error updating workflow",
			fmt.Sprintf("workflow %s no longer exists in Jira", state.ID.ValueString()))
		return
	}

	// Transitions left out of the configuration keep what Jira holds.
	var configuredTransitions types.Map
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("transitions"), &configuredTransitions)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var transitions map[string]workflowTransitionValue
	if configuredTransitions.IsNull() || plan.Transitions.IsNull() || plan.Transitions.IsUnknown() {
		transitions, err = workflowTransitionsFromAPI(*live, liveIDs)
		if err != nil {
			resp.Diagnostics.AddError("Error reading workflow transitions", err.Error())
			return
		}
	} else {
		resp.Diagnostics.Append(plan.Transitions.ElementsAs(ctx, &transitions, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	statuses, err := r.getStatuses(ctx, statusIDs)
	if err != nil {
		resp.Diagnostics.AddError("Error reading workflow statuses", err.Error())
		return
	}

	body := newWorkflowUpdateRequest(live, liveIDs, plan.Description.ValueString(), statuses, transitions)

	resp.Diagnostics.Append(r.validateUpdate(ctx, body, live, liveIDs, statusIDs)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var result workflowUpdateResponse
	if err := r.client.Post(ctx, "/rest/api/3/workflows/update", body, &result); err != nil {
		resp.Diagnostics.AddError("Error updating workflow", err.Error())
		return
	}
	if result.TaskID != nil && *result.TaskID != "" {
		if err := r.client.WaitForTask(ctx, *result.TaskID); err != nil {
			resp.Diagnostics.AddError("Error updating workflow",
				fmt.Sprintf("Jira accepted the update and continued it in task %s, which did not complete: %s",
					*result.TaskID, err))
			return
		}
	}

	found, foundIDs, err := r.fetchWorkflow(ctx, workflowReadRequest{WorkflowIDs: []string{live.ID}})
	if err != nil {
		resp.Diagnostics.AddError("Error reading updated workflow", err.Error())
		return
	}
	if found == nil {
		resp.Diagnostics.AddError("Error reading updated workflow",
			fmt.Sprintf("workflow %s was updated but cannot be read back", live.ID))
		return
	}

	resp.Diagnostics.Append(applyWorkflowRead(ctx, &plan, found, foundIDs)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// validateUpdate has Jira check the update without making it. Jira's errors
// stop the update; its warnings are passed on. A missing status mapping means
// the update removes a status from a workflow that projects use, which this
// provider does not support yet, so it gets an error of its own.
func (r *workflowResource) validateUpdate(ctx context.Context, body workflowUpdateRequest, live *workflowReadItem,
	liveIDs map[string]string, planned []string) diag.Diagnostics {
	var diags diag.Diagnostics

	request := workflowUpdateValidationRequest{
		Payload:           body,
		ValidationOptions: workflowValidationOptions{Levels: []string{"ERROR", "WARNING"}},
	}
	var result workflowValidationResponse
	if err := r.client.Post(ctx, "/rest/api/3/workflows/update/validation", request, &result); err != nil {
		diags.AddError("Error validating workflow update", "Nothing was changed in Jira. "+err.Error())
		return diags
	}

	var problems []string
	needsMappings := false
	for _, e := range result.Errors {
		if e.Level == "WARNING" {
			diags.AddWarning(fmt.Sprintf("Jira warns about workflow %q", live.Name), e.String())
			continue
		}
		if e.Type == "STATUS_MAPPING" {
			needsMappings = true
		}
		problems = append(problems, "- "+e.String())
	}
	if len(problems) == 0 {
		return diags
	}

	jiraSaid := "Jira found these problems:\n" + strings.Join(problems, "\n")
	if needsMappings {
		diags.AddAttributeError(path.Root("statuses"),
			"Removing a status from a workflow in use is not supported yet",
			fmt.Sprintf("The update removes the statuses %s from workflow %q. Projects use the workflow, "+
				"so Jira needs status mappings that move each project's issues out of those statuses, even "+
				"when none are in them. This version of the provider does not send status mappings, so "+
				"nothing was changed in Jira. Keep the statuses in the configuration, or remove them in "+
				"Jira's workflow editor first and then here.\n\n%s",
				strings.Join(removedStatusIDs(live, liveIDs, planned), ", "), live.Name, jiraSaid))
		return diags
	}

	diags.AddError(fmt.Sprintf("Jira rejected the update of workflow %q", live.Name),
		"Nothing was changed in Jira. "+jiraSaid)
	return diags
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
