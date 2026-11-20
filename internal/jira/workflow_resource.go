package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

var (
	_ resource.Resource                = &workflowResource{}
	_ resource.ResourceWithImportState = &workflowResource{}
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
		Description: "Manages a Jira Cloud workflow (structure only: name, description, statuses). Transitions are not managed in v1.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "The entity ID (UUID) of the workflow.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Description: "The name of the workflow. Changing this forces recreation of the resource.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"description": schema.StringAttribute{
				Description: "The description of the workflow. Changing this forces recreation of the resource.",
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(""),
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"statuses": schema.ListAttribute{
				Description: "List of status reference UUIDs used by the workflow. Changing this forces recreation of the resource.",
				Required:    true,
				ElementType: types.StringType,
				PlanModifiers: []planmodifier.List{
					listplanmodifier.RequiresReplace(),
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

	// Only take the ID from the response; preserve all other plan values.
	plan.ID = types.StringValue(result.Workflows[0].ID)

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

	found, err := r.findWorkflowByID(ctx, state.ID.ValueString(), state.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error reading workflow", err.Error())
		return
	}

	if found == nil {
		resp.State.RemoveResource(ctx)
		return
	}

	state.ID = types.StringValue(found.ID.EntityID)
	state.Name = types.StringValue(found.ID.Name)
	state.Description = types.StringValue(found.Description)

	// Extract status IDs from the API response. Use the numeric ID (which
	// matches the status IDs in the configuration) rather than
	// StatusReference (a UUID assigned by the server).
	statusRefs := make([]string, len(found.Statuses))
	for i, s := range found.Statuses {
		statusRefs[i] = s.ID
	}

	statusList, diags := types.ListValueFrom(ctx, types.StringType, statusRefs)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	state.Statuses = statusList

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// findWorkflowByID paginates the workflow search endpoint and returns the
// workflow matching the given entity ID, or nil if not found.
func (r *workflowResource) findWorkflowByID(ctx context.Context, entityID, name string) (*workflowAPIItem, error) {
	// When the name is known, filter by it to reduce result set.
	var apiPath string
	if name != "" {
		apiPath = fmt.Sprintf("/rest/api/3/workflow/search?workflowName=%s&expand=statuses",
			atlassian.QueryEscape(name))
	} else {
		apiPath = "/rest/api/3/workflow/search?expand=statuses"
	}

	allValues, err := r.client.GetAllPages(ctx, apiPath)
	if err != nil {
		return nil, err
	}

	for _, raw := range allValues {
		var item workflowAPIItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("unmarshaling workflow: %w", err)
		}
		if item.ID.EntityID == entityID {
			return &item, nil
		}
	}

	return nil, nil
}

func (r *workflowResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError(
		"Update not supported",
		"Jira workflows cannot be updated in v1. All fields are ForceNew.",
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

func (r *workflowResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
