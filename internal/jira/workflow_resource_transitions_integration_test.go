package jira_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

// transitionsTestStatus is a status the test creates with Terraform and then
// uses in a workflow it creates through the API.
type transitionsTestStatus struct {
	address  string
	name     string
	category string
	id       string
}

// restrictParams returns the parameters of a system:restrict-issue-transition
// condition that checks one permission, with the empty values Jira stores.
func restrictParams(permission string) map[string]string {
	return map[string]string{
		"accountIds":            "",
		"roleIds":               "",
		"groupIds":              "",
		"permissionKeys":        permission,
		"groupCustomFields":     "",
		"allowUserCustomFields": "",
		"denyUserCustomFields":  "",
	}
}

// createTransitionsTestWorkflow creates, through the bulk create API, a
// workflow with an initial, two directed and one global transition, with
// validators, post functions, properties and three levels of conditions.
func createTransitionsTestWorkflow(name, description string, todo, doing, done transitionsTestStatus) (string, error) {
	client, err := testutil.SweepClient()
	if err != nil {
		return "", err
	}

	rule := func(key string, params map[string]string) map[string]interface{} {
		return map[string]interface{}{"ruleKey": key, "parameters": params}
	}
	noRules := []interface{}{}
	statuses := []transitionsTestStatus{todo, doing, done}
	statusDefs := make([]map[string]interface{}, len(statuses))
	statusRefs := make([]map[string]interface{}, len(statuses))
	for i, s := range statuses {
		statusDefs[i] = map[string]interface{}{
			"id": s.id, "name": s.name, "description": "", "statusCategory": s.category, "statusReference": s.id,
		}
		statusRefs[i] = map[string]interface{}{"statusReference": s.id, "properties": map[string]string{}}
	}

	body := map[string]interface{}{
		"scope":    map[string]string{"type": "GLOBAL"},
		"statuses": statusDefs,
		"workflows": []interface{}{map[string]interface{}{
			"name":        name,
			"description": description,
			"statuses":    statusRefs,
			"transitions": []interface{}{
				map[string]interface{}{
					"id": "1", "name": "Create", "type": "INITIAL", "toStatusReference": todo.id,
					"links":      []interface{}{},
					"validators": []interface{}{rule("system:check-permission-validator", map[string]string{"permissionKey": "CREATE_ISSUES"})},
					"actions":    noRules, "triggers": noRules, "properties": map[string]string{},
				},
				map[string]interface{}{
					"id": "11", "name": "Start progress", "type": "DIRECTED", "toStatusReference": doing.id,
					"links": []interface{}{map[string]string{"fromStatusReference": todo.id}},
					"conditions": map[string]interface{}{
						"operation":  "ALL",
						"conditions": []interface{}{rule("system:restrict-issue-transition", restrictParams("TRANSITION_ISSUES"))},
						"conditionGroups": []interface{}{map[string]interface{}{
							"operation":  "ANY",
							"conditions": []interface{}{rule("system:restrict-issue-transition", restrictParams("RESOLVE_ISSUES"))},
							"conditionGroups": []interface{}{map[string]interface{}{
								"operation":       "ALL",
								"conditions":      []interface{}{rule("system:restrict-issue-transition", restrictParams("CLOSE_ISSUES"))},
								"conditionGroups": []interface{}{},
							}},
						}},
					},
					"validators": noRules, "actions": noRules, "triggers": noRules,
					"properties": map[string]string{"tf-acc-test.marker": "start"},
				},
				map[string]interface{}{
					"id": "21", "name": "Done", "type": "DIRECTED", "toStatusReference": done.id,
					"links": []interface{}{
						map[string]string{"fromStatusReference": todo.id},
						map[string]string{"fromStatusReference": doing.id},
					},
					"validators": []interface{}{rule("system:check-permission-validator", map[string]string{"permissionKey": "RESOLVE_ISSUES"})},
					"actions":    []interface{}{rule("system:update-field", map[string]string{"field": "resolution", "value": "10000", "mode": "replace"})},
					"triggers":   noRules, "properties": map[string]string{},
				},
				map[string]interface{}{
					"id": "31", "name": "Reopen", "description": "Back to the start", "type": "GLOBAL", "toStatusReference": todo.id,
					"links":      []interface{}{},
					"validators": noRules,
					"actions":    []interface{}{rule("system:update-field", map[string]string{"field": "resolution", "value": "", "mode": ""})},
					"triggers":   noRules, "properties": map[string]string{},
				},
			},
		}},
	}

	var result struct {
		Workflows []struct {
			ID string `json:"id"`
		} `json:"workflows"`
	}
	if err := client.Post(context.Background(), "/rest/api/3/workflows/create", body, &result); err != nil {
		return "", fmt.Errorf("creating workflow %q: %w", name, err)
	}
	if len(result.Workflows) != 1 || result.Workflows[0].ID == "" {
		return "", fmt.Errorf("creating workflow %q: got %d workflows", name, len(result.Workflows))
	}
	return result.Workflows[0].ID, nil
}

// transitionsTestStatusesConfig creates the statuses one at a time: Jira
// answers 409 "Failed to acquire lock" to statuses created in parallel.
func transitionsTestStatusesConfig(name string) string {
	return fmt.Sprintf(`
resource "atlassian_jira_status" "todo" {
  name            = "%[1]s-todo"
  description     = ""
  status_category = "TODO"
}

resource "atlassian_jira_status" "doing" {
  name            = "%[1]s-doing"
  description     = ""
  status_category = "IN_PROGRESS"
  depends_on      = [atlassian_jira_status.todo]
}

resource "atlassian_jira_status" "done" {
  name            = "%[1]s-done"
  description     = ""
  status_category = "DONE"
  depends_on      = [atlassian_jira_status.doing]
}
`, name)
}

// transitionsTestWorkflowConfig is the configuration that matches the workflow
// createTransitionsTestWorkflow creates.
func transitionsTestWorkflowConfig(name, description string) string {
	return transitionsTestStatusesConfig(name) + fmt.Sprintf(`
locals {
  restrict_empty = {
    accountIds            = ""
    roleIds               = ""
    groupIds              = ""
    groupCustomFields     = ""
    allowUserCustomFields = ""
    denyUserCustomFields  = ""
  }
}

resource "atlassian_jira_workflow" "test" {
  name        = %q
  description = %q
  statuses = [
    atlassian_jira_status.todo.id,
    atlassian_jira_status.doing.id,
    atlassian_jira_status.done.id,
  ]

  transitions = {
    "1" = {
      name = "Create"
      type = "INITIAL"
      to   = atlassian_jira_status.todo.id
      validators = [{
        rule_key   = "system:check-permission-validator"
        parameters = { permissionKey = "CREATE_ISSUES" }
      }]
    }

    "11" = {
      name = "Start progress"
      type = "DIRECTED"
      from = [atlassian_jira_status.todo.id]
      to   = atlassian_jira_status.doing.id
      conditions = {
        operation = "ALL"
        rules = [{
          rule_key   = "system:restrict-issue-transition"
          parameters = merge(local.restrict_empty, { permissionKeys = "TRANSITION_ISSUES" })
        }]
        groups = [{
          operation = "ANY"
          rules = [{
            rule_key   = "system:restrict-issue-transition"
            parameters = merge(local.restrict_empty, { permissionKeys = "RESOLVE_ISSUES" })
          }]
          groups = [{
            operation = "ALL"
            rules = [{
              rule_key   = "system:restrict-issue-transition"
              parameters = merge(local.restrict_empty, { permissionKeys = "CLOSE_ISSUES" })
            }]
          }]
        }]
      }
      properties = { "tf-acc-test.marker" = "start" }
    }

    "21" = {
      name = "Done"
      type = "DIRECTED"
      from = [atlassian_jira_status.todo.id, atlassian_jira_status.doing.id]
      to   = atlassian_jira_status.done.id
      validators = [{
        rule_key   = "system:check-permission-validator"
        parameters = { permissionKey = "RESOLVE_ISSUES" }
      }]
      actions = [{
        rule_key   = "system:update-field"
        parameters = { field = "resolution", value = "10000", mode = "replace" }
      }]
    }

    "31" = {
      name        = "Reopen"
      description = "Back to the start"
      type        = "GLOBAL"
      to          = atlassian_jira_status.todo.id
      actions = [{
        rule_key   = "system:update-field"
        parameters = { field = "resolution", value = "", mode = "" }
      }]
    }
  }
}
`, name, description)
}

// TestIntegrationWorkflowResource_importTransitions creates a workflow with
// transitions and rules outside Terraform, imports it by name and by ID, and
// checks that the matching configuration plans no changes.
func TestIntegrationWorkflowResource_importTransitions(t *testing.T) {
	testutil.SkipIfNoAcc(t)
	rName := acctest.RandomWithPrefix("tf-acc-test")
	description := fmt.Sprintf("Integration test workflow with transitions (run %s)", testutil.RunID())
	const address = "atlassian_jira_workflow.test"

	todo := transitionsTestStatus{address: "atlassian_jira_status.todo", name: rName + "-todo", category: "TODO"}
	doing := transitionsTestStatus{address: "atlassian_jira_status.doing", name: rName + "-doing", category: "IN_PROGRESS"}
	done := transitionsTestStatus{address: "atlassian_jira_status.done", name: rName + "-done", category: "DONE"}
	var workflowID string

	// Delete the workflow if a failed step left it out of state. Destroy
	// deletes it otherwise, and then this finds nothing.
	t.Cleanup(func() {
		if workflowID == "" {
			return
		}
		client, err := testutil.SweepClient()
		if err != nil {
			t.Logf("cleanup: %s", err)
			return
		}
		if _, err := client.DeleteWithStatus(context.Background(),
			"/rest/api/3/workflow/"+atlassian.PathEscape(workflowID)); err != nil {
			t.Logf("cleanup: deleting workflow %s: %s", workflowID, err)
		}
	})

	config := transitionsTestWorkflowConfig(rName, description)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		CheckDestroy:             testCheckWorkflowDestroyed,
		Steps: []resource.TestStep{
			// The statuses come first, so the workflow can use their IDs.
			{
				Config: transitionsTestStatusesConfig(rName),
				Check: func(s *terraform.State) error {
					for _, st := range []*transitionsTestStatus{&todo, &doing, &done} {
						rs, ok := s.RootModule().Resources[st.address]
						if !ok {
							return fmt.Errorf("%s not in state", st.address)
						}
						st.id = rs.Primary.ID
					}
					return nil
				},
			},
			// Create the workflow outside Terraform and import it by name.
			{
				PreConfig: func() {
					id, err := createTransitionsTestWorkflow(rName, description, todo, doing, done)
					if err != nil {
						t.Fatal(err)
					}
					workflowID = id
				},
				Config:             config,
				ResourceName:       address,
				ImportState:        true,
				ImportStateId:      rName,
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					for _, st := range states {
						if st.Attributes["name"] != rName {
							continue
						}
						if st.ID != workflowID {
							return fmt.Errorf("imported id %q, want %q", st.ID, workflowID)
						}
						if st.Attributes["transitions.%"] != "4" {
							return fmt.Errorf("imported %s transitions, want 4", st.Attributes["transitions.%"])
						}
						return nil
					}
					return fmt.Errorf("workflow %q not imported", rName)
				},
			},
			// The matching configuration plans no changes.
			{
				Config:   config,
				PlanOnly: true,
			},
			// An import block by ID plans the import and nothing else.
			{
				Config:          config,
				ResourceName:    address,
				ImportState:     true,
				ImportStateKind: resource.ImportBlockWithID,
				ImportStateIdFunc: func(*terraform.State) (string, error) {
					return workflowID, nil
				},
			},
		},
	})
}
