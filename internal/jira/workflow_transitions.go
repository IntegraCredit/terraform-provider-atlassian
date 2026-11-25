package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// transitionIDPattern is the form Jira requires of a transition ID.
var transitionIDPattern = regexp.MustCompile(`^[0-9]+$`)

// maxConditionDepth is how many levels of condition groups the schema holds:
// a transition's own group, the groups in it, and the groups in those.
// Jira itself sets no limit, but a schema cannot refer to itself.
const maxConditionDepth = 3

// API types for POST /rest/api/3/workflows, the bulk workflow read.

type workflowReadRequest struct {
	WorkflowIDs   []string `json:"workflowIds,omitempty"`
	WorkflowNames []string `json:"workflowNames,omitempty"`
}

type workflowReadResponse struct {
	Statuses  []workflowReadStatus `json:"statuses"`
	Workflows []workflowReadItem   `json:"workflows"`
}

// workflowReadStatus maps a status reference used inside the workflow to the
// status's ID. For an existing global status the two are the same.
type workflowReadStatus struct {
	ID              string `json:"id"`
	StatusReference string `json:"statusReference"`
}

// workflowReadItem is a workflow as the bulk read returns it. The bulk update
// takes the same shapes back, so the fields the provider does not manage, such
// as the diagram layout, are kept here as Jira sends them.
type workflowReadItem struct {
	ID                              string                  `json:"id"`
	Name                            string                  `json:"name"`
	Description                     string                  `json:"description"`
	Version                         workflowVersionAPI      `json:"version"`
	StartPointLayout                json.RawMessage         `json:"startPointLayout"`
	LoopedTransitionContainerLayout json.RawMessage         `json:"loopedTransitionContainerLayout"`
	Statuses                        []workflowStatusAPI     `json:"statuses"`
	Transitions                     []workflowTransitionAPI `json:"transitions"`
}

// workflowVersionAPI is a workflow's version. An update must name the current one.
type workflowVersionAPI struct {
	ID            string `json:"id"`
	VersionNumber int64  `json:"versionNumber"`
}

// workflowStatusAPI is a status as one workflow uses it: its place in the
// diagram, its properties and, in service projects, its approval.
type workflowStatusAPI struct {
	StatusReference       string          `json:"statusReference"`
	Layout                json.RawMessage `json:"layout,omitempty"`
	Properties            json.RawMessage `json:"properties"`
	ApprovalConfiguration json.RawMessage `json:"approvalConfiguration,omitempty"`
}

type workflowTransitionAPI struct {
	ID                 string                      `json:"id"`
	Name               string                      `json:"name"`
	Description        string                      `json:"description"`
	Type               string                      `json:"type"`
	ToStatusReference  string                      `json:"toStatusReference,omitempty"`
	Links              []workflowTransitionLinkAPI `json:"links"`
	Conditions         *workflowConditionGroupAPI  `json:"conditions,omitempty"`
	Validators         []workflowRuleAPI           `json:"validators"`
	Actions            []workflowRuleAPI           `json:"actions"`
	Triggers           []workflowRuleAPI           `json:"triggers"`
	Properties         map[string]string           `json:"properties"`
	TransitionScreen   json.RawMessage             `json:"transitionScreen,omitempty"`
	CustomIssueEventID json.RawMessage             `json:"customIssueEventId,omitempty"`
}

// workflowTransitionLinkAPI is one status a transition starts from, with the
// ports its arrow uses in the diagram. An INITIAL or GLOBAL transition's link
// has no fromStatusReference and only places the arrow.
type workflowTransitionLinkAPI struct {
	FromStatusReference string `json:"fromStatusReference,omitempty"`
	FromPort            *int   `json:"fromPort,omitempty"`
	ToPort              *int   `json:"toPort,omitempty"`
}

// workflowRuleAPI is a condition, validator, post function or trigger. Jira
// generates its "id", so the ID is not part of the configuration; an update
// sends it back for a rule that stays, or Jira would give the rule a new one.
type workflowRuleAPI struct {
	ID         string            `json:"id,omitempty"`
	RuleKey    string            `json:"ruleKey"`
	Parameters map[string]string `json:"parameters"`
}

type workflowConditionGroupAPI struct {
	Operation       string                      `json:"operation"`
	Conditions      []workflowRuleAPI           `json:"conditions"`
	ConditionGroups []workflowConditionGroupAPI `json:"conditionGroups"`
}

// Terraform values. The framework converts these Go types to the schema's
// types: a nil pointer becomes null, so every slice and map here is non-nil
// to match the schema's empty defaults.

type workflowTransitionValue struct {
	Name        string                   `tfsdk:"name"`
	Description string                   `tfsdk:"description"`
	Type        string                   `tfsdk:"type"`
	From        []string                 `tfsdk:"from"`
	To          *string                  `tfsdk:"to"`
	Conditions  *workflowConditionGroup1 `tfsdk:"conditions"`
	Validators  []workflowRuleValue      `tfsdk:"validators"`
	Actions     []workflowRuleValue      `tfsdk:"actions"`
	Triggers    []workflowRuleValue      `tfsdk:"triggers"`
	Properties  map[string]string        `tfsdk:"properties"`
}

type workflowRuleValue struct {
	RuleKey    string            `tfsdk:"rule_key"`
	Parameters map[string]string `tfsdk:"parameters"`
}

// workflowConditionGroup1 is a transition's own condition group.
type workflowConditionGroup1 struct {
	Operation string                    `tfsdk:"operation"`
	Rules     []workflowRuleValue       `tfsdk:"rules"`
	Groups    []workflowConditionGroup2 `tfsdk:"groups"`
}

// workflowConditionGroup2 is a group inside the transition's group.
type workflowConditionGroup2 struct {
	Operation string                    `tfsdk:"operation"`
	Rules     []workflowRuleValue       `tfsdk:"rules"`
	Groups    []workflowConditionGroup3 `tfsdk:"groups"`
}

// workflowConditionGroup3 is the deepest group the schema holds.
type workflowConditionGroup3 struct {
	Operation string              `tfsdk:"operation"`
	Rules     []workflowRuleValue `tfsdk:"rules"`
}

type workflowVersionValue struct {
	ID     string `tfsdk:"id"`
	Number int64  `tfsdk:"number"`
}

// Schema.

var workflowRuleObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"rule_key":   types.StringType,
	"parameters": types.MapType{ElemType: types.StringType},
}}

var workflowVersionAttrTypes = map[string]attr.Type{
	"id":     types.StringType,
	"number": types.Int64Type,
}

func emptyStringMapDefault() map[string]attr.Value {
	return map[string]attr.Value{}
}

func workflowRuleListAttribute(description string) schema.ListNestedAttribute {
	return schema.ListNestedAttribute{
		Description: description + " Defaults to none.",
		Optional:    true,
		Computed:    true,
		Default:     listdefault.StaticValue(types.ListValueMust(workflowRuleObjectType, []attr.Value{})),
		NestedObject: schema.NestedAttributeObject{
			Attributes: map[string]schema.Attribute{
				"rule_key": schema.StringAttribute{
					Description: "The rule's key, for example `system:update-field`.",
					Required:    true,
				},
				"parameters": schema.MapAttribute{
					Description: "The rule's parameters, exactly as Jira stores them. Jira often stores " +
						"parameters with empty values; list those too. Defaults to none.",
					ElementType: types.StringType,
					Optional:    true,
					Computed:    true,
					Default:     mapdefault.StaticValue(types.MapValueMust(types.StringType, emptyStringMapDefault())),
				},
			},
		},
	}
}

// workflowConditionGroupAttributes returns the attributes of a condition group
// at the given level, 1 being the transition's own group. Below the deepest
// level the schema holds, a group has no "groups".
func workflowConditionGroupAttributes(level int) map[string]schema.Attribute {
	attrs := map[string]schema.Attribute{
		"operation": schema.StringAttribute{
			Description: "`ALL` passes when every rule and group in this group passes; `ANY` when at least one does.",
			Required:    true,
			Validators:  []validator.String{stringvalidator.OneOf("ALL", "ANY")},
		},
		"rules": workflowRuleListAttribute("The conditions in this group."),
	}
	if level < maxConditionDepth {
		nested := schema.NestedAttributeObject{Attributes: workflowConditionGroupAttributes(level + 1)}
		attrs["groups"] = schema.ListNestedAttribute{
			Description: fmt.Sprintf("Condition groups nested in this group. The provider holds %d levels of "+
				"groups; reading a workflow nested deeper fails. Defaults to none.", maxConditionDepth),
			Optional:     true,
			Computed:     true,
			Default:      listdefault.StaticValue(types.ListValueMust(nested.Type(), []attr.Value{})),
			NestedObject: nested,
		}
	}
	return attrs
}

// workflowTransitionsAttribute is the schema of the "transitions" attribute.
func workflowTransitionsAttribute() schema.MapNestedAttribute {
	return schema.MapNestedAttribute{
		Description: "The workflow's transitions, keyed by transition ID. Leave it out to ignore the " +
			"transitions; they are still read into state, and an update keeps them as Jira holds them. " +
			"When set, it is the whole list: a transition left out is removed. Keep the IDs of existing " +
			"transitions; a new transition needs a new numeric ID, such as the highest ID plus 10.",
		Optional: true,
		Computed: true,
		PlanModifiers: []planmodifier.Map{
			mapplanmodifier.UseStateForUnknown(),
		},
		Validators: []validator.Map{
			mapvalidator.KeysAre(stringvalidator.RegexMatches(transitionIDPattern,
				"must be a transition ID: digits only, as Jira requires")),
		},
		NestedObject: schema.NestedAttributeObject{
			Attributes: map[string]schema.Attribute{
				"name": schema.StringAttribute{
					Description: "The transition's name.",
					Required:    true,
				},
				"description": schema.StringAttribute{
					Description: "The transition's description. Defaults to an empty string.",
					Optional:    true,
					Computed:    true,
					Default:     stringdefault.StaticString(""),
				},
				"type": schema.StringAttribute{
					Description: "`INITIAL` (creates the issue), `DIRECTED` (from the statuses in `from`) " +
						"or `GLOBAL` (from any status).",
					Required:   true,
					Validators: []validator.String{stringvalidator.OneOf("INITIAL", "DIRECTED", "GLOBAL")},
				},
				"from": schema.SetAttribute{
					Description: "The IDs of the statuses a `DIRECTED` transition starts from. Empty for " +
						"`INITIAL` and `GLOBAL` transitions, which is the default.",
					ElementType: types.StringType,
					Optional:    true,
					Computed:    true,
					Default:     setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{})),
				},
				"to": schema.StringAttribute{
					Description: "The ID of the status the transition goes to.",
					Optional:    true,
				},
				"conditions": schema.SingleNestedAttribute{
					Description: "The transition's conditions, as a group of rules and nested groups. " +
						"Jira reads a group with no rules and no groups as no conditions.",
					Optional:   true,
					Attributes: workflowConditionGroupAttributes(1),
				},
				"validators": workflowRuleListAttribute("The transition's validators."),
				"actions":    workflowRuleListAttribute("The transition's post functions, in the order they run."),
				"triggers":   workflowRuleListAttribute("The transition's triggers."),
				"properties": schema.MapAttribute{
					Description: "The transition's properties. Defaults to none.",
					ElementType: types.StringType,
					Optional:    true,
					Computed:    true,
					Default:     mapdefault.StaticValue(types.MapValueMust(types.StringType, emptyStringMapDefault())),
				},
			},
		},
	}
}

// workflowTransitionObjectType is the type of one transition, taken from the
// schema so the two cannot drift apart.
func workflowTransitionObjectType() attr.Type {
	return workflowTransitionsAttribute().NestedObject.Type()
}

// Conversion from the API.

// workflowStatusIDs maps each status reference in a read response to its ID.
func workflowStatusIDs(statuses []workflowReadStatus) map[string]string {
	ids := make(map[string]string, len(statuses))
	for _, s := range statuses {
		if s.StatusReference != "" && s.ID != "" {
			ids[s.StatusReference] = s.ID
		}
	}
	return ids
}

func statusIDFor(ids map[string]string, ref string) string {
	if id, ok := ids[ref]; ok {
		return id
	}
	return ref
}

// workflowStatusesFromAPI returns the workflow's status IDs in Jira's order.
func workflowStatusesFromAPI(item workflowReadItem, ids map[string]string) []string {
	statuses := make([]string, len(item.Statuses))
	for i, s := range item.Statuses {
		statuses[i] = statusIDFor(ids, s.StatusReference)
	}
	return statuses
}

// workflowTransitionsFromAPI converts a workflow's transitions to Terraform
// values keyed by transition ID.
func workflowTransitionsFromAPI(item workflowReadItem, ids map[string]string) (map[string]workflowTransitionValue, error) {
	transitions := make(map[string]workflowTransitionValue, len(item.Transitions))
	for _, t := range item.Transitions {
		if _, dup := transitions[t.ID]; dup {
			return nil, fmt.Errorf("workflow %q has two transitions with ID %q", item.Name, t.ID)
		}

		conditions, err := conditionGroup1FromAPI(t.Conditions)
		if err != nil {
			return nil, fmt.Errorf("transition %q (%s): %w", t.Name, t.ID, err)
		}

		from := []string{}
		for _, link := range t.Links {
			if link.FromStatusReference == "" {
				continue
			}
			id := statusIDFor(ids, link.FromStatusReference)
			if !slices.Contains(from, id) {
				from = append(from, id)
			}
		}
		slices.Sort(from)

		var to *string
		if t.ToStatusReference != "" {
			id := statusIDFor(ids, t.ToStatusReference)
			to = &id
		}

		transitions[t.ID] = workflowTransitionValue{
			Name:        t.Name,
			Description: t.Description,
			Type:        t.Type,
			From:        from,
			To:          to,
			Conditions:  conditions,
			Validators:  rulesFromAPI(t.Validators),
			Actions:     rulesFromAPI(t.Actions),
			Triggers:    rulesFromAPI(t.Triggers),
			Properties:  stringMapOrEmpty(t.Properties),
		}
	}
	return transitions, nil
}

func rulesFromAPI(rules []workflowRuleAPI) []workflowRuleValue {
	values := make([]workflowRuleValue, len(rules))
	for i, r := range rules {
		values[i] = workflowRuleValue{RuleKey: r.RuleKey, Parameters: stringMapOrEmpty(r.Parameters)}
	}
	return values
}

func stringMapOrEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func conditionGroupEmpty(g *workflowConditionGroupAPI) bool {
	return g == nil || (len(g.Conditions) == 0 && len(g.ConditionGroups) == 0)
}

func conditionGroup1FromAPI(g *workflowConditionGroupAPI) (*workflowConditionGroup1, error) {
	if conditionGroupEmpty(g) {
		return nil, nil
	}
	groups := make([]workflowConditionGroup2, len(g.ConditionGroups))
	for i, sub := range g.ConditionGroups {
		v, err := conditionGroup2FromAPI(sub)
		if err != nil {
			return nil, err
		}
		groups[i] = v
	}
	return &workflowConditionGroup1{Operation: g.Operation, Rules: rulesFromAPI(g.Conditions), Groups: groups}, nil
}

func conditionGroup2FromAPI(g workflowConditionGroupAPI) (workflowConditionGroup2, error) {
	groups := make([]workflowConditionGroup3, len(g.ConditionGroups))
	for i, sub := range g.ConditionGroups {
		if len(sub.ConditionGroups) > 0 {
			return workflowConditionGroup2{}, fmt.Errorf(
				"its condition groups are nested more than %d levels deep, which this provider cannot hold",
				maxConditionDepth)
		}
		groups[i] = workflowConditionGroup3{Operation: sub.Operation, Rules: rulesFromAPI(sub.Conditions)}
	}
	return workflowConditionGroup2{Operation: g.Operation, Rules: rulesFromAPI(g.Conditions), Groups: groups}, nil
}

// sameStatusSet reports whether two status lists hold the same IDs, in any order.
func sameStatusSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa, sb := slices.Clone(a), slices.Clone(b)
	slices.Sort(sa)
	slices.Sort(sb)
	return slices.Equal(sa, sb)
}

// workflowTransitionsMapValue converts transitions to the schema's map type.
func workflowTransitionsMapValue(ctx context.Context, transitions map[string]workflowTransitionValue) (types.Map, error) {
	value, diags := types.MapValueFrom(ctx, workflowTransitionObjectType(), transitions)
	if diags.HasError() {
		return types.MapNull(workflowTransitionObjectType()), fmt.Errorf("converting transitions: %v", diags)
	}
	return value, nil
}
