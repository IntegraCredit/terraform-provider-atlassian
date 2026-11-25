package jira

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// API types for POST /rest/api/3/workflows/update and
// POST /rest/api/3/workflows/update/validation.

// workflowUpdateRequest updates workflows. Statuses lists every status the
// workflows use, each sent as it is now, so the update leaves it unchanged.
type workflowUpdateRequest struct {
	Statuses  []workflowCreateStatus `json:"statuses"`
	Workflows []workflowUpdateItem   `json:"workflows"`
}

// workflowUpdateItem is the whole of one workflow. Jira replaces the workflow
// with it: a status, transition, layout or rule ID left out is removed or reset.
// Jira ignores a name here, so the update cannot rename a workflow.
type workflowUpdateItem struct {
	ID                              string                  `json:"id"`
	Description                     string                  `json:"description"`
	Version                         workflowVersionAPI      `json:"version"`
	StartPointLayout                json.RawMessage         `json:"startPointLayout,omitempty"`
	LoopedTransitionContainerLayout json.RawMessage         `json:"loopedTransitionContainerLayout,omitempty"`
	Statuses                        []workflowStatusAPI     `json:"statuses"`
	Transitions                     []workflowTransitionAPI `json:"transitions"`
}

type workflowUpdateValidationRequest struct {
	Payload           workflowUpdateRequest     `json:"payload"`
	ValidationOptions workflowValidationOptions `json:"validationOptions"`
}

type workflowValidationOptions struct {
	Levels []string `json:"levels"`
}

type workflowValidationResponse struct {
	Errors []workflowValidationError `json:"errors"`
}

// workflowValidationError is one problem Jira finds in an update. Level is
// ERROR or WARNING.
type workflowValidationError struct {
	Type             string `json:"type"`
	Code             string `json:"code"`
	Level            string `json:"level"`
	Message          string `json:"message"`
	ElementReference *struct {
		StatusReference string `json:"statusReference"`
		TransitionID    string `json:"transitionId"`
		RuleID          string `json:"ruleId"`
		PropertyKey     string `json:"propertyKey"`
	} `json:"elementReference"`
}

// workflowUpdateResponse is the part of the update response the provider
// reads. Jira returns a task ID when the update goes on in the background.
type workflowUpdateResponse struct {
	TaskID *string `json:"taskId"`
}

// String describes the problem and the element it is about.
func (e workflowValidationError) String() string {
	var where []string
	if ref := e.ElementReference; ref != nil {
		if ref.TransitionID != "" {
			where = append(where, "transition "+ref.TransitionID)
		}
		if ref.StatusReference != "" {
			where = append(where, "status "+ref.StatusReference)
		}
		if ref.RuleID != "" {
			where = append(where, "rule "+ref.RuleID)
		}
		if ref.PropertyKey != "" {
			where = append(where, "property "+ref.PropertyKey)
		}
	}
	s := e.Message
	if len(where) > 0 {
		s = strings.Join(where, ", ") + ": " + s
	}
	return fmt.Sprintf("%s (%s %s)", s, e.Type, e.Code)
}

// newWorkflowUpdateRequest builds the update that gives the live workflow the
// planned description, statuses and transitions. Because Jira replaces the
// whole workflow, the request also carries over, from the live workflow, what
// the provider does not manage: the diagram layout and the ports of arrows
// that stay, status properties and approvals, transition screens and events,
// and the IDs of rules that stay in place. statuses are the planned statuses
// in order, as GET /rest/api/3/statuses returns them; liveIDs maps the live
// workflow's status references to status IDs.
func newWorkflowUpdateRequest(live *workflowReadItem, liveIDs map[string]string, description string,
	statuses []statusAPIItem, transitions map[string]workflowTransitionValue) workflowUpdateRequest {
	liveStatuses := make(map[string]workflowStatusAPI, len(live.Statuses))
	for _, s := range live.Statuses {
		liveStatuses[statusIDFor(liveIDs, s.StatusReference)] = s
	}

	statusDefs := make([]workflowCreateStatus, len(statuses))
	workflowStatuses := make([]workflowStatusAPI, len(statuses))
	for i, s := range statuses {
		statusDefs[i] = workflowCreateStatus{
			ID:              s.ID,
			Name:            s.Name,
			Description:     s.Description,
			StatusCategory:  s.statusCategoryKey(),
			StatusReference: s.ID,
		}
		ws := workflowStatusAPI{StatusReference: s.ID, Properties: json.RawMessage(`{}`)}
		if prev, ok := liveStatuses[s.ID]; ok {
			ws.Layout = rawOrNil(prev.Layout)
			if props := rawOrNil(prev.Properties); props != nil {
				ws.Properties = props
			}
			ws.ApprovalConfiguration = rawOrNil(prev.ApprovalConfiguration)
		}
		workflowStatuses[i] = ws
	}

	liveTransitions := make(map[string]*workflowTransitionAPI, len(live.Transitions))
	for i := range live.Transitions {
		liveTransitions[live.Transitions[i].ID] = &live.Transitions[i]
	}

	ids := make([]string, 0, len(transitions))
	for id := range transitions {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, compareTransitionIDs)

	apiTransitions := make([]workflowTransitionAPI, len(ids))
	for i, id := range ids {
		apiTransitions[i] = transitionToAPI(id, transitions[id], liveTransitions[id], liveIDs)
	}

	return workflowUpdateRequest{
		Statuses: statusDefs,
		Workflows: []workflowUpdateItem{{
			ID:                              live.ID,
			Description:                     description,
			Version:                         live.Version,
			StartPointLayout:                rawOrNil(live.StartPointLayout),
			LoopedTransitionContainerLayout: rawOrNil(live.LoopedTransitionContainerLayout),
			Statuses:                        workflowStatuses,
			Transitions:                     apiTransitions,
		}},
	}
}

// transitionToAPI converts a planned transition. prev is the live transition
// with the same ID, or nil for a new one.
func transitionToAPI(id string, t workflowTransitionValue, prev *workflowTransitionAPI, liveIDs map[string]string) workflowTransitionAPI {
	var prevValidators, prevActions, prevTriggers []workflowRuleAPI
	var prevConditions *workflowConditionGroupAPI
	if prev != nil {
		prevValidators, prevActions, prevTriggers = prev.Validators, prev.Actions, prev.Triggers
		prevConditions = prev.Conditions
	}

	api := workflowTransitionAPI{
		ID:          id,
		Name:        t.Name,
		Description: t.Description,
		Type:        t.Type,
		Links:       transitionLinksToAPI(t.From, prev, liveIDs),
		Conditions:  conditionGroup1ToAPI(t.Conditions, prevConditions),
		Validators:  rulesToAPI(t.Validators, prevValidators),
		Actions:     rulesToAPI(t.Actions, prevActions),
		Triggers:    rulesToAPI(t.Triggers, prevTriggers),
		Properties:  stringMapOrEmpty(t.Properties),
	}
	if t.To != nil {
		api.ToStatusReference = *t.To
	}
	if prev != nil {
		api.TransitionScreen = rawOrNil(prev.TransitionScreen)
		api.CustomIssueEventID = rawOrNil(prev.CustomIssueEventID)
	}
	return api
}

// transitionLinksToAPI returns one link per status the transition starts
// from, keeping the ports of a link the live transition already has. A
// transition from no status keeps the live links that only place its arrow.
func transitionLinksToAPI(from []string, prev *workflowTransitionAPI, liveIDs map[string]string) []workflowTransitionLinkAPI {
	links := []workflowTransitionLinkAPI{}
	if len(from) == 0 {
		if prev != nil {
			for _, l := range prev.Links {
				if l.FromStatusReference == "" {
					links = append(links, l)
				}
			}
		}
		return links
	}

	prevLinks := map[string]workflowTransitionLinkAPI{}
	if prev != nil {
		for _, l := range prev.Links {
			if l.FromStatusReference != "" {
				prevLinks[statusIDFor(liveIDs, l.FromStatusReference)] = l
			}
		}
	}

	sorted := slices.Clone(from)
	slices.Sort(sorted)
	for _, f := range sorted {
		link := workflowTransitionLinkAPI{FromStatusReference: f}
		if p, ok := prevLinks[f]; ok {
			link.FromPort, link.ToPort = p.FromPort, p.ToPort
		}
		links = append(links, link)
	}
	return links
}

// rulesToAPI converts rules, keeping the ID of the live rule at the same
// position when it has the same key.
func rulesToAPI(values []workflowRuleValue, prev []workflowRuleAPI) []workflowRuleAPI {
	rules := make([]workflowRuleAPI, len(values))
	for i, v := range values {
		rules[i] = workflowRuleAPI{RuleKey: v.RuleKey, Parameters: stringMapOrEmpty(v.Parameters)}
		if i < len(prev) && prev[i].RuleKey == v.RuleKey {
			rules[i].ID = prev[i].ID
		}
	}
	return rules
}

// conditionGroupAt returns the live group at index i, or nil.
func conditionGroupAt(groups []workflowConditionGroupAPI, i int) *workflowConditionGroupAPI {
	if i < len(groups) {
		return &groups[i]
	}
	return nil
}

func conditionGroup1ToAPI(g *workflowConditionGroup1, prev *workflowConditionGroupAPI) *workflowConditionGroupAPI {
	if g == nil {
		return nil
	}
	var prevRules []workflowRuleAPI
	var prevGroups []workflowConditionGroupAPI
	if prev != nil {
		prevRules, prevGroups = prev.Conditions, prev.ConditionGroups
	}
	groups := make([]workflowConditionGroupAPI, len(g.Groups))
	for i, sub := range g.Groups {
		groups[i] = conditionGroup2ToAPI(sub, conditionGroupAt(prevGroups, i))
	}
	return &workflowConditionGroupAPI{Operation: g.Operation, Conditions: rulesToAPI(g.Rules, prevRules), ConditionGroups: groups}
}

func conditionGroup2ToAPI(g workflowConditionGroup2, prev *workflowConditionGroupAPI) workflowConditionGroupAPI {
	var prevRules []workflowRuleAPI
	var prevGroups []workflowConditionGroupAPI
	if prev != nil {
		prevRules, prevGroups = prev.Conditions, prev.ConditionGroups
	}
	groups := make([]workflowConditionGroupAPI, len(g.Groups))
	for i, sub := range g.Groups {
		var prevSubRules []workflowRuleAPI
		if p := conditionGroupAt(prevGroups, i); p != nil {
			prevSubRules = p.Conditions
		}
		groups[i] = workflowConditionGroupAPI{
			Operation:       sub.Operation,
			Conditions:      rulesToAPI(sub.Rules, prevSubRules),
			ConditionGroups: []workflowConditionGroupAPI{},
		}
	}
	return workflowConditionGroupAPI{Operation: g.Operation, Conditions: rulesToAPI(g.Rules, prevRules), ConditionGroups: groups}
}

// compareTransitionIDs orders transition IDs as numbers. Jira's are numeric;
// anything else sorts after them as text.
func compareTransitionIDs(a, b string) int {
	na, errA := strconv.Atoi(a)
	nb, errB := strconv.Atoi(b)
	switch {
	case errA == nil && errB == nil:
		return cmp.Compare(na, nb)
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// rawOrNil returns nil for an absent, null or empty JSON object, so the
// request leaves such a value out rather than sending it back.
func rawOrNil(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
		return nil
	}
	return raw
}

// removedStatusIDs returns the live workflow's statuses that the plan drops,
// in the live workflow's order.
func removedStatusIDs(live *workflowReadItem, liveIDs map[string]string, planned []string) []string {
	var removed []string
	for _, s := range live.Statuses {
		id := statusIDFor(liveIDs, s.StatusReference)
		if !slices.Contains(planned, id) {
			removed = append(removed, id)
		}
	}
	return removed
}
