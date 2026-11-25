package jira

import (
	"encoding/json"
	"fmt"
)

// ApplyWorkflowUpdate returns a bulk read response as Jira gives it after a
// bulk update: the first workflow in read takes the description, statuses,
// layout and transitions of the first workflow in update, its version number
// goes up by one with a new version ID, and every rule sent without an ID gets
// a new one. The response's statuses are the update's. The mocks in this
// package and in jira_test use it as Jira's side of the update.
func ApplyWorkflowUpdate(read, update []byte) ([]byte, error) {
	var doc, body map[string]interface{}
	if err := json.Unmarshal(read, &doc); err != nil {
		return nil, fmt.Errorf("decoding read: %w", err)
	}
	if err := json.Unmarshal(update, &body); err != nil {
		return nil, fmt.Errorf("decoding update: %w", err)
	}

	workflows, _ := doc["workflows"].([]interface{})
	updates, _ := body["workflows"].([]interface{})
	if len(workflows) == 0 || len(updates) == 0 {
		return nil, fmt.Errorf("no workflow to update")
	}
	wf := workflows[0].(map[string]interface{})
	upd := updates[0].(map[string]interface{})

	version := wf["version"].(map[string]interface{})
	number := version["versionNumber"].(float64) + 1
	wf["version"] = map[string]interface{}{
		"id":            fmt.Sprintf("00000000-0000-0000-0000-%012d", int(number)),
		"versionNumber": number,
	}
	wf["description"] = upd["description"]
	wf["startPointLayout"] = valueOrEmptyObject(upd["startPointLayout"])

	var statuses []interface{}
	for _, s := range upd["statuses"].([]interface{}) {
		sm := s.(map[string]interface{})
		statuses = append(statuses, map[string]interface{}{
			"statusReference": sm["statusReference"],
			"layout":          valueOrEmptyObject(sm["layout"]),
			"properties":      valueOrEmptyObject(sm["properties"]),
			"deprecated":      false,
		})
	}
	wf["statuses"] = statuses

	ids := 0
	newID := func() string {
		ids++
		return fmt.Sprintf("generated-rule-%d", ids)
	}
	transitions := upd["transitions"].([]interface{})
	for _, t := range transitions {
		tm := t.(map[string]interface{})
		for _, key := range []string{"validators", "actions", "triggers"} {
			assignRuleIDs(tm[key], newID)
		}
		if c, ok := tm["conditions"].(map[string]interface{}); ok {
			assignConditionIDs(c, newID)
		}
	}
	wf["transitions"] = transitions

	var defs []interface{}
	for _, s := range body["statuses"].([]interface{}) {
		sm := s.(map[string]interface{})
		defs = append(defs, map[string]interface{}{
			"id": sm["id"], "statusReference": sm["statusReference"], "name": sm["name"],
			"statusCategory": sm["statusCategory"], "description": sm["description"],
			"scope": map[string]interface{}{"type": "GLOBAL"},
		})
	}
	doc["statuses"] = defs

	return json.Marshal(doc)
}

func valueOrEmptyObject(v interface{}) interface{} {
	if v == nil {
		return map[string]interface{}{}
	}
	return v
}

func assignRuleIDs(rules interface{}, newID func() string) {
	list, _ := rules.([]interface{})
	for _, r := range list {
		rm := r.(map[string]interface{})
		if id, _ := rm["id"].(string); id == "" {
			rm["id"] = newID()
		}
	}
}

func assignConditionIDs(group map[string]interface{}, newID func() string) {
	assignRuleIDs(group["conditions"], newID)
	subs, _ := group["conditionGroups"].([]interface{})
	for _, sub := range subs {
		assignConditionIDs(sub.(map[string]interface{}), newID)
	}
}
