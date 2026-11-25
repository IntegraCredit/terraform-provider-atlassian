package jira_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

// rawWorkflow is a workflow from the bulk read, kept as JSON values so a test
// can compare what the provider does not model.
type rawWorkflow map[string]interface{}

func readRawWorkflow(id string) (rawWorkflow, error) {
	client, err := testutil.SweepClient()
	if err != nil {
		return nil, err
	}
	var result struct {
		Workflows []rawWorkflow `json:"workflows"`
	}
	if err := client.Post(context.Background(), "/rest/api/3/workflows",
		map[string][]string{"workflowIds": {id}}, &result); err != nil {
		return nil, fmt.Errorf("reading workflow %s: %w", id, err)
	}
	if len(result.Workflows) != 1 {
		return nil, fmt.Errorf("reading workflow %s: got %d workflows", id, len(result.Workflows))
	}
	return result.Workflows[0], nil
}

func (w rawWorkflow) versionNumber() float64 {
	return w["version"].(map[string]interface{})["versionNumber"].(float64)
}

func (w rawWorkflow) transition(id string) map[string]interface{} {
	for _, t := range w["transitions"].([]interface{}) {
		if tm := t.(map[string]interface{}); tm["id"] == id {
			return tm
		}
	}
	return nil
}

// statusLayouts maps each status reference to its layout.
func (w rawWorkflow) statusLayouts() map[string]interface{} {
	layouts := map[string]interface{}{}
	for _, s := range w["statuses"].([]interface{}) {
		sm := s.(map[string]interface{})
		layouts[sm["statusReference"].(string)] = sm["layout"]
	}
	return layouts
}

// ruleIDs lists the IDs of a transition's rules, conditions included, in a
// fixed order.
func ruleIDs(t map[string]interface{}) []string {
	var ids []string
	var collect func(v interface{})
	collect = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			if id, ok := x["id"].(string); ok && x["ruleKey"] != nil {
				ids = append(ids, id)
			}
			for _, key := range []string{"validators", "actions", "triggers", "conditions", "conditionGroups"} {
				collect(x[key])
			}
		case []interface{}:
			for _, e := range x {
				collect(e)
			}
		}
	}
	collect(map[string]interface{}{
		"validators": t["validators"], "actions": t["actions"], "triggers": t["triggers"], "conditions": t["conditions"],
	})
	return ids
}

// updateTestStatusesConfig is transitionsTestStatusesConfig with a fourth
// status, review, which the workflow does not use at first.
func updateTestStatusesConfig(name string) string {
	return transitionsTestStatusesConfig(name) + fmt.Sprintf(`
resource "atlassian_jira_status" "review" {
  name            = "%s-review"
  description     = ""
  status_category = "IN_PROGRESS"
  depends_on      = [atlassian_jira_status.done]
}
`, name)
}

// updateTestReviewConfig adds the review status to the workflow with
// transitions into and out of it, and changes the permission transition 21's
// validator checks.
func updateTestReviewConfig(name, description string) string {
	config := updateTestStatusesConfig(name) + transitionsTestWorkflowBlock(name, description,
		"    atlassian_jira_status.review.id,\n", `
    "41" = {
      name = "Request review"
      type = "DIRECTED"
      from = [atlassian_jira_status.doing.id]
      to   = atlassian_jira_status.review.id
    }

    "51" = {
      name        = "Approve"
      description = "Review passed"
      type        = "DIRECTED"
      from        = [atlassian_jira_status.review.id]
      to          = atlassian_jira_status.done.id
      validators = [{
        rule_key   = "system:check-permission-validator"
        parameters = { permissionKey = "RESOLVE_ISSUES" }
      }]
    }
`)
	return strings.Replace(config, `parameters = { permissionKey = "RESOLVE_ISSUES" }
      }]
      actions = [{`, `parameters = { permissionKey = "TRANSITION_ISSUES" }
      }]
      actions = [{`, 1)
}

// TestIntegrationWorkflowResource_update imports a workflow, adds a status
// with transitions into and out of it and changes a validator, then removes
// the status again. Each change is an in-place update that plans no changes
// once applied. The update keeps what the provider does not manage: the
// diagram layout, the arrow ports and the IDs of rules that stay. No project
// uses the workflow, so removing a status needs no status mappings.
func TestIntegrationWorkflowResource_update(t *testing.T) {
	testutil.SkipIfNoAcc(t)
	rName := acctest.RandomWithPrefix("tf-acc-test")
	description := fmt.Sprintf("Integration test workflow update (run %s)", testutil.RunID())
	const address = "atlassian_jira_workflow.test"

	todo := transitionsTestStatus{address: "atlassian_jira_status.todo", name: rName + "-todo", category: "TODO"}
	doing := transitionsTestStatus{address: "atlassian_jira_status.doing", name: rName + "-doing", category: "IN_PROGRESS"}
	done := transitionsTestStatus{address: "atlassian_jira_status.done", name: rName + "-done", category: "DONE"}
	var workflowID string
	var before rawWorkflow

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

	original := updateTestStatusesConfig(rName) + transitionsTestWorkflowBlock(rName, description, "", "")
	review := updateTestReviewConfig(rName, description)
	expectUpdate := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate)},
	}

	// keptUnmanaged checks that the update kept the layout, the ports and the
	// IDs of the rules in transitions 1, 11, 21 and 31.
	keptUnmanaged := func(*terraform.State) error {
		after, err := readRawWorkflow(workflowID)
		if err != nil {
			return err
		}
		if after.versionNumber() <= before.versionNumber() {
			return fmt.Errorf("version %v did not go up from %v", after.versionNumber(), before.versionNumber())
		}
		if !reflect.DeepEqual(after["startPointLayout"], before["startPointLayout"]) {
			return fmt.Errorf("start point layout %v, want %v", after["startPointLayout"], before["startPointLayout"])
		}
		afterLayouts := after.statusLayouts()
		for ref, layout := range before.statusLayouts() {
			if !reflect.DeepEqual(afterLayouts[ref], layout) {
				return fmt.Errorf("status %s layout %v, want %v", ref, afterLayouts[ref], layout)
			}
		}
		for _, id := range []string{"1", "11", "21", "31"} {
			if got, want := ruleIDs(after.transition(id)), ruleIDs(before.transition(id)); !slices.Equal(got, want) {
				return fmt.Errorf("transition %s rule IDs %v, want %v", id, got, want)
			}
		}
		links, _ := json.Marshal(after.transition("21")["links"])
		if want := fmt.Sprintf(`{"fromPort":2,"fromStatusReference":%q,"toPort":6}`, todo.id); !strings.Contains(string(links), want) {
			return fmt.Errorf("transition 21 links %s, want the arrow from todo to keep its ports", links)
		}
		return nil
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		CheckDestroy:             testCheckWorkflowDestroyed,
		Steps: []resource.TestStep{
			{
				Config: updateTestStatusesConfig(rName),
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
			{
				PreConfig: func() {
					id, err := createTransitionsTestWorkflow(rName, description, todo, doing, done)
					if err != nil {
						t.Fatal(err)
					}
					workflowID = id
				},
				Config:             original,
				ResourceName:       address,
				ImportState:        true,
				ImportStateId:      rName,
				ImportStatePersist: true,
			},
			{
				Config:   original,
				PlanOnly: true,
			},
			// Add the review status and two transitions, and change a validator.
			{
				PreConfig: func() {
					raw, err := readRawWorkflow(workflowID)
					if err != nil {
						t.Fatal(err)
					}
					before = raw
				},
				Config:           review,
				ConfigPlanChecks: expectUpdate,
				Check: resource.ComposeAggregateTestCheckFunc(
					func(s *terraform.State) error {
						if got := s.RootModule().Resources[address].Primary.ID; got != workflowID {
							return fmt.Errorf("id %q, want %q", got, workflowID)
						}
						return nil
					},
					resource.TestCheckResourceAttr(address, "statuses.#", "4"),
					resource.TestCheckResourceAttr(address, "transitions.%", "6"),
					resource.TestCheckResourceAttrPair(address, "transitions.41.to", "atlassian_jira_status.review", "id"),
					resource.TestCheckResourceAttr(address, "transitions.21.validators.0.parameters.permissionKey", "TRANSITION_ISSUES"),
					keptUnmanaged,
				),
			},
			{
				Config:   review,
				PlanOnly: true,
			},
			// Remove the review status again.
			{
				Config:           original,
				ConfigPlanChecks: expectUpdate,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(address, "statuses.#", "3"),
					resource.TestCheckResourceAttr(address, "transitions.%", "4"),
				),
			},
			{
				Config:   original,
				PlanOnly: true,
			},
		},
	})
}

// inUseTestConfig is a workflow that a project uses through a workflow
// scheme. stage 0 is the workflow as the provider creates it, with todo and
// doing and the initial transition; stage 1 adds transitions between them;
// stage 2 adds done with transitions into and out of it. Without the project,
// it leaves out the project and its scheme assignment.
func inUseTestConfig(name, projectKey, leadAccountID string, stage int, withProject bool) string {
	statuses := "atlassian_jira_status.todo.id, atlassian_jira_status.doing.id"
	transitions := ""
	if stage >= 1 {
		more := ""
		if stage >= 2 {
			statuses += ", atlassian_jira_status.done.id"
			more = `
    "31" = {
      name = "Finish"
      type = "DIRECTED"
      from = [atlassian_jira_status.doing.id]
      to   = atlassian_jira_status.done.id
    }

    "41" = {
      name = "Reopen"
      type = "DIRECTED"
      from = [atlassian_jira_status.done.id]
      to   = atlassian_jira_status.todo.id
    }
`
		}
		transitions = `
  transitions = {
    "1" = {
      name = "Create"
      type = "INITIAL"
      to   = atlassian_jira_status.todo.id
    }

    "11" = {
      name = "Start"
      type = "DIRECTED"
      from = [atlassian_jira_status.todo.id]
      to   = atlassian_jira_status.doing.id
    }

    "21" = {
      name = "Reset"
      type = "GLOBAL"
      to   = atlassian_jira_status.todo.id
    }
` + more + `  }
`
	}

	config := transitionsTestStatusesConfig(name) + fmt.Sprintf(`
resource "atlassian_jira_workflow" "test" {
  name        = %[1]q
  description = "Integration test (run %[2]s)"
  statuses    = [%[3]s]
%[4]s}

resource "atlassian_jira_workflow_scheme" "test" {
  name             = %[1]q
  description      = "Integration test (run %[2]s)"
  default_workflow = atlassian_jira_workflow.test.name
}
`, name, testutil.RunID(), statuses, transitions)
	if !withProject {
		return config
	}
	return config + fmt.Sprintf(`
resource "atlassian_jira_project" "test" {
  key              = %q
  name             = %q
  project_type_key = "software"
  lead_account_id  = %q
}

resource "atlassian_jira_project_workflow_scheme" "test" {
  project_id         = atlassian_jira_project.test.id
  workflow_scheme_id = atlassian_jira_workflow_scheme.test.id
}
`, projectKey, name, leadAccountID)
}

// inUseObjects are the IDs of what the in-use test creates, for its cleanup.
type inUseObjects struct {
	project, scheme, workflow string
	statuses                  []string
}

// record takes the IDs from state.
func (o *inUseObjects) record(s *terraform.State) error {
	id := func(address string) (string, error) {
		rs, ok := s.RootModule().Resources[address]
		if !ok {
			return "", fmt.Errorf("%s not in state", address)
		}
		return rs.Primary.ID, nil
	}
	var err error
	if o.project, err = id("atlassian_jira_project.test"); err != nil {
		return err
	}
	if o.scheme, err = id("atlassian_jira_workflow_scheme.test"); err != nil {
		return err
	}
	if o.workflow, err = id("atlassian_jira_workflow.test"); err != nil {
		return err
	}
	o.statuses = nil
	for _, name := range []string{"todo", "doing", "done"} {
		statusID, err := id("atlassian_jira_status." + name)
		if err != nil {
			return err
		}
		o.statuses = append(o.statuses, statusID)
	}
	return nil
}

// deleteProject deletes the project for good, with its issues. A project in
// the trash still holds its workflow scheme, and the test user may not delete
// issues, so this is how the test lets go of the scheme.
func (o *inUseObjects) deleteProject(client *atlassian.Client) error {
	if o.project == "" {
		return nil
	}
	if _, err := client.DeleteWithStatus(context.Background(),
		"/rest/api/3/project/"+atlassian.PathEscape(o.project)+"?enableUndo=false"); err != nil {
		return fmt.Errorf("deleting project %s: %w", o.project, err)
	}
	o.project = ""
	return nil
}

// cleanup deletes whatever a failed run left. The destroy cannot delete the
// workflow scheme while the project uses it, nor what the scheme depends on.
// After a clean run everything is gone and Jira answers 404.
func (o *inUseObjects) cleanup(t *testing.T, client *atlassian.Client) {
	if err := o.deleteProject(client); err != nil {
		t.Logf("cleanup: %s", err)
	}
	paths := []string{}
	if o.scheme != "" {
		paths = append(paths, "/rest/api/3/workflowscheme/"+atlassian.PathEscape(o.scheme))
	}
	if o.workflow != "" {
		paths = append(paths, "/rest/api/3/workflow/"+atlassian.PathEscape(o.workflow))
	}
	for _, id := range o.statuses {
		paths = append(paths, "/rest/api/3/statuses?id="+atlassian.QueryEscape(id))
	}
	for _, p := range paths {
		if _, err := client.DeleteWithStatus(context.Background(), p); err != nil {
			t.Logf("cleanup: %s", err)
		}
	}
}

// inUseIssue is an issue the in-use test creates and moves through the workflow.
type inUseIssue struct {
	client *atlassian.Client
	key    string
}

func (i *inUseIssue) create(projectKey string) error {
	ctx := context.Background()
	var project struct {
		IssueTypes []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Subtask bool   `json:"subtask"`
		} `json:"issueTypes"`
	}
	if err := i.client.Get(ctx, "/rest/api/3/project/"+atlassian.PathEscape(projectKey), &project); err != nil {
		return fmt.Errorf("reading project %s: %w", projectKey, err)
	}
	issueType := ""
	for _, it := range project.IssueTypes {
		if !it.Subtask && (issueType == "" || it.Name == "Task") {
			issueType = it.ID
		}
	}
	if issueType == "" {
		return fmt.Errorf("project %s has no standard issue type", projectKey)
	}

	var created struct {
		Key string `json:"key"`
	}
	body := map[string]interface{}{"fields": map[string]interface{}{
		"project": map[string]string{"key": projectKey}, "issuetype": map[string]string{"id": issueType},
		"summary": "Workflow update integration test",
	}}
	if err := i.client.Post(ctx, "/rest/api/3/issue", body, &created); err != nil {
		return fmt.Errorf("creating issue: %w", err)
	}
	i.key = created.Key
	return nil
}

func (i *inUseIssue) transition(id string) error {
	body := map[string]interface{}{"transition": map[string]string{"id": id}}
	if err := i.client.Post(context.Background(), "/rest/api/3/issue/"+atlassian.PathEscape(i.key)+"/transitions", body, nil); err != nil {
		return fmt.Errorf("moving %s through transition %s: %w", i.key, id, err)
	}
	return nil
}

func (i *inUseIssue) status() (string, error) {
	var issue struct {
		Fields struct {
			Status struct {
				ID string `json:"id"`
			} `json:"status"`
		} `json:"fields"`
	}
	if err := i.client.Get(context.Background(), "/rest/api/3/issue/"+atlassian.PathEscape(i.key)+"?fields=status", &issue); err != nil {
		return "", fmt.Errorf("reading %s: %w", i.key, err)
	}
	return issue.Fields.Status.ID, nil
}

// TestIntegrationWorkflowResource_updateInUse changes a workflow that a
// project uses through a workflow scheme and that has an issue: it adds a
// status with transitions into and out of it. Jira applies the update to the
// live workflow, with no draft workflow scheme, and the issue can take the new
// transitions at once. Removing the status again needs status mappings, so it
// fails before Jira changes anything, and so does a new name.
func TestIntegrationWorkflowResource_updateInUse(t *testing.T) {
	testutil.SkipIfNoAcc(t)
	rName := acctest.RandomWithPrefix("tf-acc-test")
	projectKey := "TFACC" + acctest.RandStringFromCharSet(5, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	const address = "atlassian_jira_workflow.test"

	client, err := testutil.SweepClient()
	if err != nil {
		t.Fatalf("creating client: %s", err)
	}
	leadAccountID, err := getTestAccountID(context.Background(), client)
	if err != nil {
		t.Fatalf("getting account ID: %s", err)
	}
	issue := &inUseIssue{client: client}
	objects := &inUseObjects{}
	t.Cleanup(func() { objects.cleanup(t, client) })

	config := func(stage int) string { return inUseTestConfig(rName, projectKey, leadAccountID, stage, true) }
	expectUpdate := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate)},
	}

	// noDraftScheme checks that the update left the workflow scheme without a draft.
	noDraftScheme := func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources["atlassian_jira_workflow_scheme.test"]
		if !ok {
			return fmt.Errorf("workflow scheme not in state")
		}
		var draft map[string]interface{}
		status, err := client.GetWithStatus(context.Background(),
			"/rest/api/3/workflowscheme/"+atlassian.PathEscape(rs.Primary.ID)+"/draft", &draft)
		if err != nil {
			return err
		}
		if status != http.StatusNotFound {
			return fmt.Errorf("workflow scheme %s has a draft: %v", rs.Primary.ID, draft)
		}
		return nil
	}

	// issueTakesNewTransition moves the issue from doing to done through the new transition 31.
	issueTakesNewTransition := func(s *terraform.State) error {
		if err := issue.transition("31"); err != nil {
			return err
		}
		got, err := issue.status()
		if err != nil {
			return err
		}
		if want := s.RootModule().Resources["atlassian_jira_status.done"].Primary.ID; got != want {
			return fmt.Errorf("%s is in status %s, want done (%s)", issue.key, got, want)
		}
		return nil
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		// The destroy deleted everything, so the cleanup has nothing left to do.
		CheckDestroy: func(s *terraform.State) error {
			if err := testCheckWorkflowDestroyed(s); err != nil {
				return err
			}
			*objects = inUseObjects{}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: config(0),
				Check: resource.ComposeAggregateTestCheckFunc(
					objects.record,
					resource.TestCheckResourceAttr(address, "transitions.%", "1"),
					resource.TestCheckResourceAttrPair(
						"atlassian_jira_project_workflow_scheme.test", "workflow_scheme_id",
						"atlassian_jira_workflow_scheme.test", "id"),
				),
			},
			// Add transitions to the workflow the project now uses.
			{
				Config:           config(1),
				ConfigPlanChecks: expectUpdate,
				Check:            resource.TestCheckResourceAttr(address, "transitions.%", "3"),
			},
			// With an issue in doing, add done and transitions into and out of it.
			{
				PreConfig: func() {
					if err := issue.create(projectKey); err != nil {
						t.Fatal(err)
					}
					if err := issue.transition("11"); err != nil {
						t.Fatal(err)
					}
				},
				Config:           config(2),
				ConfigPlanChecks: expectUpdate,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(address, "statuses.#", "3"),
					resource.TestCheckResourceAttr(address, "transitions.%", "5"),
					noDraftScheme,
					issueTakesNewTransition,
				),
			},
			{
				Config:   config(2),
				PlanOnly: true,
			},
			// Removing done from the workflow in use fails before Jira changes it.
			{
				Config:      config(1),
				ExpectError: regexp.MustCompile(`Removing a status from a workflow in use is not supported yet`),
			},
			{
				Config:   config(2),
				PlanOnly: true,
			},
			// Jira's API cannot rename a workflow, so the plan fails.
			{
				Config:      strings.Replace(config(2), fmt.Sprintf("name        = %q", rName), fmt.Sprintf("name        = %q", rName+"-renamed"), 1),
				ExpectError: regexp.MustCompile(`Renaming a workflow is not supported`),
			},
			// Delete the project for good, so the destroy can delete the
			// workflow scheme. The refresh finds the project and its scheme
			// assignment gone, and the rest has nothing to change.
			{
				PreConfig: func() {
					if err := objects.deleteProject(client); err != nil {
						t.Fatal(err)
					}
				},
				Config:           inUseTestConfig(rName, projectKey, leadAccountID, 2, false),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}
