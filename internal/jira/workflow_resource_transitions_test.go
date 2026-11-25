package jira_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/jira"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

const (
	transitionsFixtureID   = "4d8f5a78-e5e2-4357-8757-5b04a3c719e7"
	transitionsFixtureName = "tf-test-workflow-transitions"
)

// newWorkflowFixtureServer serves testdata/workflow_bulk_read.json as an
// existing workflow: the bulk read finds it by ID or name, the bulk update
// changes it as Jira would, and DELETE removes it. The status lookup knows the
// fixture's statuses and one more, 10200.
func newWorkflowFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	fixture, err := os.ReadFile("testdata/workflow_bulk_read.json")
	if err != nil {
		t.Fatalf("reading fixture: %s", err)
	}

	var doc struct {
		Statuses []map[string]interface{} `json:"statuses"`
	}
	if err := json.Unmarshal(fixture, &doc); err != nil {
		t.Fatalf("decoding fixture: %s", err)
	}
	statuses := map[string]map[string]interface{}{
		"10200": {"id": "10200", "name": "Merged", "description": "", "statusCategory": "IN_PROGRESS"},
	}
	for _, s := range doc.Statuses {
		statuses[s["id"].(string)] = s
	}

	var mu sync.Mutex
	deleted := false
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/workflows":
			var body struct {
				WorkflowIDs   []string `json:"workflowIds"`
				WorkflowNames []string `json:"workflowNames"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			named := slices.Contains(body.WorkflowIDs, transitionsFixtureID) ||
				slices.Contains(body.WorkflowNames, transitionsFixtureName)
			if deleted || !named {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(fixture) //nolint:errcheck

		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/statuses":
			found := []map[string]interface{}{}
			for _, id := range r.URL.Query()["id"] {
				if st, ok := statuses[id]; ok {
					found = append(found, st)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(found) //nolint:errcheck

		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/workflows/update/validation":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"errors":[]}`)) //nolint:errcheck

		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/workflows/update":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			updated, err := jira.ApplyWorkflowUpdate(fixture, body)
			if err != nil {
				t.Errorf("applying update: %s", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fixture = updated
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"taskId":null}`)) //nolint:errcheck

		case r.Method == http.MethodDelete && r.URL.Path == "/rest/api/3/workflow/"+transitionsFixtureID:
			deleted = true
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}

// transitionsFixtureConfig is the configuration that matches the fixture.
// doneName renames transition 31; name renames the workflow.
func transitionsFixtureConfig(name, doneName string) string {
	return transitionsFixtureConfigWith(name, doneName, `"10000", "3", "10002", "10100"`, "")
}

// transitionsFixtureMergedConfig adds status 10200 and transitions 41 into it
// and 51 out of it to the fixture's configuration.
func transitionsFixtureMergedConfig(doneName string) string {
	return transitionsFixtureConfigWith(transitionsFixtureName, doneName, `"10000", "3", "10002", "10100", "10200"`, `
    "41" = {
      name = "Merge"
      type = "DIRECTED"
      from = ["10100"]
      to   = "10200"
    }

    "51" = {
      name = "Ship"
      type = "DIRECTED"
      from = ["10200"]
      to   = "10002"
    }
`)
}

// transitionsFixtureConfigWith is the fixture's configuration with the given
// statuses and more transitions.
func transitionsFixtureConfigWith(name, doneName, statuses, moreTransitions string) string {
	return fmt.Sprintf(`
resource "atlassian_jira_workflow" "test" {
  name        = %q
  description = "Workflow with transitions"
  statuses    = [%s]

  transitions = {
    "1" = {
      name = "Create"
      type = "INITIAL"
      to   = "10000"
      validators = [{
        rule_key   = "system:check-permission-validator"
        parameters = { permissionKey = "CREATE_ISSUES" }
      }]
      properties = { "jira.i18n.title" = "common.forms.create" }
    }

    "21" = {
      name        = "Request review"
      description = "Send for review"
      type        = "DIRECTED"
      from        = ["3"]
      to          = "10100"
      triggers = [{
        rule_key   = "system:pull-request-created-trigger"
        parameters = { enabled = "true" }
      }]
      conditions = {
        operation = "ALL"
        rules = [{
          rule_key = "system:restrict-issue-transition"
          parameters = {
            accountIds            = "allow-assignee"
            roleIds               = ""
            groupIds              = ""
            permissionKeys        = ""
            groupCustomFields     = ""
            allowUserCustomFields = ""
            denyUserCustomFields  = ""
          }
        }]
        groups = [{
          operation = "ANY"
          rules = [{
            rule_key   = "system:restrict-issue-transition"
            parameters = { permissionKeys = "RESOLVE_ISSUES" }
          }]
          groups = [{
            operation = "ALL"
            rules = [{
              rule_key   = "system:restrict-issue-transition"
              parameters = { permissionKeys = "CLOSE_ISSUES" }
            }]
          }]
        }]
      }
    }

    "31" = {
      name = %q
      type = "DIRECTED"
      from = ["3", "10100", "10000"]
      to   = "10002"
      actions = [{
        rule_key   = "system:update-field"
        parameters = { field = "resolution", value = "10000", mode = "replace" }
      }]
    }

    "11" = {
      name = "Open"
      type = "GLOBAL"
      to   = "10000"
      actions = [{
        rule_key   = "system:update-field"
        parameters = { field = "resolution", value = "", mode = "" }
      }]
    }
%s  }
}
`, name, statuses, doneName, moreTransitions)
}

// TestAccWorkflowResource_ImportTransitions imports an existing workflow with
// transitions, rules and three levels of conditions, by ID and by name, and
// checks that the matching configuration plans no changes. A changed
// transition and a new status each plan an in-place update, never a
// replacement, that applies and then plans no changes. A new name and a
// transition ID Jira would reject fail the plan.
func TestAccWorkflowResource_ImportTransitions(t *testing.T) {
	mockServer := newWorkflowFixtureServer(t)
	defer mockServer.Close()

	t.Setenv("ATLASSIAN_URL", mockServer.URL)
	t.Setenv("ATLASSIAN_USER", "test@test.com")
	t.Setenv("ATLASSIAN_TOKEN", "test-token")

	const address = "atlassian_jira_workflow.test"
	config := transitionsFixtureConfig(transitionsFixtureName, "Done")
	expectUpdate := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate)},
	}
	renamed := transitionsFixtureConfig(transitionsFixtureName+"-renamed", "Done")
	badTransitionID := strings.Replace(config, `"11" = {`, `"open" = {`, 1)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		CheckDestroy: func(s *terraform.State) error {
			return nil
		},
		Steps: []resource.TestStep{
			// Import into state, then plan the same configuration: no changes.
			{
				Config:             config,
				ResourceName:       address,
				ImportState:        true,
				ImportStateId:      transitionsFixtureName,
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("imported %d resources, want 1", len(states))
					}
					attrs := states[0].Attributes
					for key, want := range map[string]string{
						"id":                    transitionsFixtureID,
						"version.number":        "4",
						"transitions.%":         "4",
						"transitions.21.to":     "10100",
						"transitions.31.from.#": "3",
						"transitions.21.conditions.groups.0.groups.0.rules.0.parameters.permissionKeys": "CLOSE_ISSUES",
					} {
						if attrs[key] != want {
							return fmt.Errorf("%s = %q, want %q", key, attrs[key], want)
						}
					}
					return nil
				},
			},
			{
				Config:   config,
				PlanOnly: true,
			},
			// An import block by ID, then by name, plans the import and nothing
			// else. These steps plan against a copy of the state with the
			// workflow removed, so they need the import above.
			{
				Config:          config,
				ResourceName:    address,
				ImportState:     true,
				ImportStateKind: resource.ImportBlockWithID,
				ImportStateId:   transitionsFixtureID,
			},
			{
				Config:          config,
				ResourceName:    address,
				ImportState:     true,
				ImportStateKind: resource.ImportBlockWithID,
				ImportStateId:   transitionsFixtureName,
			},
			// A new name and a transition ID Jira rejects fail the plan.
			{
				Config:      renamed,
				ExpectError: regexp.MustCompile(`Renaming a workflow is not supported`),
			},
			{
				Config:      badTransitionID,
				ExpectError: regexp.MustCompile(`must be a transition ID`),
			},
			// A changed transition is an in-place update. Applied, it plans no changes.
			{
				Config:           transitionsFixtureConfig(transitionsFixtureName, "Finish"),
				ConfigPlanChecks: expectUpdate,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(address, "id", transitionsFixtureID),
					resource.TestCheckResourceAttr(address, "transitions.31.name", "Finish"),
					resource.TestCheckResourceAttr(address, "version.number", "5"),
				),
			},
			{
				Config:   transitionsFixtureConfig(transitionsFixtureName, "Finish"),
				PlanOnly: true,
			},
			// So is a new status with transitions into and out of it.
			{
				Config:           transitionsFixtureMergedConfig("Finish"),
				ConfigPlanChecks: expectUpdate,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(address, "statuses.#", "5"),
					resource.TestCheckResourceAttr(address, "statuses.4", "10200"),
					resource.TestCheckResourceAttr(address, "transitions.%", "6"),
					resource.TestCheckResourceAttr(address, "transitions.41.to", "10200"),
					resource.TestCheckResourceAttr(address, "transitions.51.from.0", "10200"),
					resource.TestCheckResourceAttr(address, "version.number", "6"),
				),
			},
			{
				Config:   transitionsFixtureMergedConfig("Finish"),
				PlanOnly: true,
			},
			// Leaving the transitions out of the configuration ignores them.
			{
				Config: `
resource "atlassian_jira_workflow" "test" {
  name        = "` + transitionsFixtureName + `"
  description = "Workflow with transitions"
  statuses    = ["10000", "3", "10002", "10100", "10200"]
}
`,
				PlanOnly: true,
			},
		},
	})
}
