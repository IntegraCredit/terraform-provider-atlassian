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

func TestIntegrationProjectIssueTypeScreenSchemeResource_basic(t *testing.T) {
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
		CheckDestroy:             testCheckIssueTypeScreenSchemeDestroyed,
		Steps: []resource.TestStep{
			{
				Config: testIntegrationProjectIssueTypeScreenSchemeConfig(projectKey, rName, leadAccountID, "test"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("atlassian_jira_project_issue_type_screen_scheme.test", "project_id"),
					resource.TestCheckResourceAttrPair(
						"atlassian_jira_project_issue_type_screen_scheme.test", "issue_type_screen_scheme_id",
						"atlassian_jira_issue_type_screen_scheme.test", "id",
					),
				),
			},
			{
				// Switching schemes must be one in-place assignment, not a replace
				// that passes through the default scheme.
				Config: testIntegrationProjectIssueTypeScreenSchemeConfig(projectKey, rName, leadAccountID, "other"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("atlassian_jira_project_issue_type_screen_scheme.test", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.TestCheckResourceAttrPair(
					"atlassian_jira_project_issue_type_screen_scheme.test", "issue_type_screen_scheme_id",
					"atlassian_jira_issue_type_screen_scheme.other", "id",
				),
			},
			{
				ResourceName:                         "atlassian_jira_project_issue_type_screen_scheme.test",
				ImportState:                          true,
				ImportStateIdFunc:                    testProjectIssueTypeScreenSchemeImportID,
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "project_id",
			},
		},
	})
}

func testProjectIssueTypeScreenSchemeImportID(s *terraform.State) (string, error) {
	rs, ok := s.RootModule().Resources["atlassian_jira_project_issue_type_screen_scheme.test"]
	if !ok {
		return "", fmt.Errorf("resource not found in state")
	}
	return rs.Primary.Attributes["project_id"], nil
}

// testIntegrationProjectIssueTypeScreenSchemeConfig creates a project and two issue
// type screen schemes, and assigns the one named by scheme ("test" or "other").
func testIntegrationProjectIssueTypeScreenSchemeConfig(projectKey, name, leadAccountID, scheme string) string {
	return fmt.Sprintf(`
resource "atlassian_jira_project" "test" {
  key              = %[1]q
  name             = %[2]q
  project_type_key = "software"
  lead_account_id  = %[3]q
}

resource "atlassian_jira_screen" "pitss_dep" {
  name = "%[2]s-screen"
}

resource "atlassian_jira_screen_scheme" "pitss_dep" {
  name = "%[2]s-screenscheme"
  screens = {
    default = atlassian_jira_screen.pitss_dep.id
  }
}

resource "atlassian_jira_issue_type_screen_scheme" "test" {
  name        = "%[2]s-itss"
  description = "Integration test (run %[4]s)"
  mappings = {
    default = atlassian_jira_screen_scheme.pitss_dep.id
  }
}

resource "atlassian_jira_issue_type_screen_scheme" "other" {
  name        = "%[2]s-itss-other"
  description = "Integration test (run %[4]s)"
  mappings = {
    default = atlassian_jira_screen_scheme.pitss_dep.id
  }
}

resource "atlassian_jira_project_issue_type_screen_scheme" "test" {
  project_id                  = atlassian_jira_project.test.id
  issue_type_screen_scheme_id = atlassian_jira_issue_type_screen_scheme.%[5]s.id
}
`, projectKey, name, leadAccountID, testutil.RunID(), scheme)
}
