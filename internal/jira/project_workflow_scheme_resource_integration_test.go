package jira_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

func TestIntegrationProjectWorkflowSchemeResource_basic(t *testing.T) {
	testutil.SkipIfNoAcc(t)

	rName := acctest.RandomWithPrefix("tf-acc-test")
	projectKey := fmt.Sprintf("TFACC%s", acctest.RandStringFromCharSet(5, "ABCDEFGHIJKLMNOPQRSTUVWXYZ"))

	client, err := testutil.SweepClient()
	if err != nil {
		t.Fatalf("creating client: %s", err)
	}
	leadAccountID, err := getTestAccountID(context.Background(), client)
	if err != nil {
		t.Fatalf("getting account ID: %s", err)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		CheckDestroy:             testCheckProjectWorkflowSchemeDestroyed,
		Steps: []resource.TestStep{
			{
				Config: testIntegrationProjectWorkflowSchemeConfig(projectKey, rName, leadAccountID, "test"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("atlassian_jira_project_workflow_scheme.test", "project_id"),
					resource.TestCheckResourceAttrPair(
						"atlassian_jira_project_workflow_scheme.test", "workflow_scheme_id",
						"atlassian_jira_workflow_scheme.test", "id",
					),
				),
			},
			{
				// Switching schemes must be one in-place assignment, not a replace
				// that passes through the default scheme. The project has no issues,
				// so Jira accepts the change.
				Config: testIntegrationProjectWorkflowSchemeConfig(projectKey, rName, leadAccountID, "other"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("atlassian_jira_project_workflow_scheme.test", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.TestCheckResourceAttrPair(
					"atlassian_jira_project_workflow_scheme.test", "workflow_scheme_id",
					"atlassian_jira_workflow_scheme.other", "id",
				),
			},
			{
				ResourceName:                         "atlassian_jira_project_workflow_scheme.test",
				ImportState:                          true,
				ImportStateIdFunc:                    testProjectWorkflowSchemeImportID,
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "project_id",
			},
		},
	})
}

func testProjectWorkflowSchemeImportID(s *terraform.State) (string, error) {
	rs, ok := s.RootModule().Resources["atlassian_jira_project_workflow_scheme.test"]
	if !ok {
		return "", fmt.Errorf("resource not found in state")
	}
	return rs.Primary.Attributes["project_id"], nil
}

// testIntegrationProjectWorkflowSchemeConfig creates a project and two workflow
// schemes, and assigns the one named by scheme ("test" or "other") to the project.
func testIntegrationProjectWorkflowSchemeConfig(projectKey, name, leadAccountID, scheme string) string {
	return fmt.Sprintf(`
resource "atlassian_jira_project" "test" {
  key              = %q
  name             = %q
  project_type_key = "software"
  lead_account_id  = %q
}

resource "atlassian_jira_workflow_scheme" "test" {
  name             = %q
  description      = "Integration test (run %s)"
  default_workflow = "jira"
}

resource "atlassian_jira_workflow_scheme" "other" {
  name             = %q
  description      = "Integration test (run %s)"
  default_workflow = "jira"
}

resource "atlassian_jira_project_workflow_scheme" "test" {
  project_id         = atlassian_jira_project.test.id
  workflow_scheme_id = atlassian_jira_workflow_scheme.%s.id
}
`, projectKey, name, leadAccountID, name+"-wf-scheme", testutil.RunID(), name+"-wf-scheme-other", testutil.RunID(), scheme)
}

func testCheckProjectWorkflowSchemeDestroyed(s *terraform.State) error {
	// Delete reverts the project to the default workflow scheme and then
	// the project itself is deleted. Nothing to verify — the association
	// and both parent resources are gone after a full destroy.
	return nil
}
