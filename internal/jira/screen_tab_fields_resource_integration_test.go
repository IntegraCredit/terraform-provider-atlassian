package jira_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

// TestIntegrationScreenTabFieldsResource_basic orders the fields of a new tab,
// reorders them in place, imports the tab, puts back a reorder made in Jira, and
// checks that destroying the resource removes its fields from the tab.
func TestIntegrationScreenTabFieldsResource_basic(t *testing.T) {
	testutil.SkipIfNoAcc(t)
	rName := acctest.RandomWithPrefix("tf-acc-test")

	const addr = "atlassian_jira_screen_tab_fields.test"
	var screenID, tabID string

	captureIDs := func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[addr]
		if !ok {
			return fmt.Errorf("%s not found in state", addr)
		}
		screenID = rs.Primary.Attributes["screen_id"]
		tabID = rs.Primary.Attributes["tab_id"]
		return nil
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		CheckDestroy:             testCheckScreenDestroyed,
		Steps: []resource.TestStep{
			{
				Config: testIntegrationScreenTabFieldsConfig(rName, "summary", "description", "labels", "environment"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureIDs,
					resource.TestCheckResourceAttrPair(addr, "screen_id", "atlassian_jira_screen.test", "id"),
					resource.TestCheckResourceAttrPair(addr, "tab_id", "atlassian_jira_screen_tab.test", "id"),
					resource.TestCheckResourceAttr(addr, "fields.#", "4"),
					testCheckJiraTabFields(&screenID, &tabID, "summary", "description", "labels", "environment"),
				),
			},
			{
				// Remove one field, add one, and move two.
				Config: testIntegrationScreenTabFieldsConfig(rName, "duedate", "summary", "labels", "description"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
					},
				},
				Check: testCheckJiraTabFields(&screenID, &tabID, "duedate", "summary", "labels", "description"),
			},
			{
				ResourceName:      addr,
				ImportState:       true,
				ImportStateIdFunc: testImportScreenTabFieldsID(addr),
				ImportStateVerify: true,
			},
			{
				// A reorder made in Jira shows as drift.
				PreConfig: func() {
					testMoveJiraTabField(t, screenID, tabID, "description", map[string]string{"position": "First"})
				},
				Config:             testIntegrationScreenTabFieldsConfig(rName, "duedate", "summary", "labels", "description"),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: testIntegrationScreenTabFieldsConfig(rName, "duedate", "summary", "labels", "description"),
				Check:  testCheckJiraTabFields(&screenID, &tabID, "duedate", "summary", "labels", "description"),
			},
			{
				// Destroying the resource removes its fields; the tab stays.
				Config: testIntegrationScreenTabFieldsConfig(rName),
				Check:  testCheckJiraTabFields(&screenID, &tabID),
			},
		},
	})
}

// testIntegrationScreenTabFieldsConfig writes a screen with one tab. With no
// fields, it leaves out the atlassian_jira_screen_tab_fields resource.
func testIntegrationScreenTabFieldsConfig(name string, fields ...string) string {
	config := fmt.Sprintf(`
resource "atlassian_jira_screen" "test" {
  name        = %q
  description = "Integration test screen (run %s)"
}

resource "atlassian_jira_screen_tab" "test" {
  screen_id = atlassian_jira_screen.test.id
  name      = %q
}
`, name, testutil.RunID(), name+"-tab")

	if len(fields) == 0 {
		return config
	}

	quoted := make([]string, len(fields))
	for i, f := range fields {
		quoted[i] = fmt.Sprintf("%q", f)
	}
	return config + fmt.Sprintf(`
resource "atlassian_jira_screen_tab_fields" "test" {
  screen_id = atlassian_jira_screen.test.id
  tab_id    = atlassian_jira_screen_tab.test.id
  fields    = [%s]
}
`, strings.Join(quoted, ", "))
}

// testCheckJiraTabFields reads the tab from Jira and compares its fields, in order.
func testCheckJiraTabFields(screenID, tabID *string, want ...string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		client, err := testutil.SweepClient()
		if err != nil {
			return fmt.Errorf("creating client: %w", err)
		}
		var fields []struct {
			ID string `json:"id"`
		}
		apiPath := fmt.Sprintf("/rest/api/3/screens/%s/tabs/%s/fields", *screenID, *tabID)
		if err := client.Get(context.Background(), apiPath, &fields); err != nil {
			return fmt.Errorf("reading tab fields: %w", err)
		}
		got := make([]string, 0, len(fields))
		for _, f := range fields {
			got = append(got, f.ID)
		}
		if !slices.Equal(got, want) {
			return fmt.Errorf("Jira lists the tab fields as %q, want %q", got, want)
		}
		return nil
	}
}

// testMoveJiraTabField moves a field on a tab outside Terraform.
func testMoveJiraTabField(t *testing.T, screenID, tabID, fieldID string, body map[string]string) {
	t.Helper()
	client, err := testutil.SweepClient()
	if err != nil {
		t.Fatalf("creating client: %s", err)
	}
	apiPath := fmt.Sprintf("/rest/api/3/screens/%s/tabs/%s/fields/%s/move", screenID, tabID, fieldID)
	if err := client.Post(context.Background(), apiPath, body, nil); err != nil {
		t.Fatalf("moving field %s: %s", fieldID, err)
	}
}

// testImportScreenTabFieldsID returns an ImportStateIdFunc that builds "{screen_id}/{tab_id}".
func testImportScreenTabFieldsID(resourceAddr string) resource.ImportStateIdFunc {
	return func(s *terraform.State) (string, error) {
		rs, ok := s.RootModule().Resources[resourceAddr]
		if !ok {
			return "", fmt.Errorf("resource %s not found in state", resourceAddr)
		}
		screenID := rs.Primary.Attributes["screen_id"]
		tabID := rs.Primary.Attributes["tab_id"]
		if screenID == "" || tabID == "" {
			return "", fmt.Errorf("screen_id or tab_id is empty for resource %s", resourceAddr)
		}
		return screenID + "/" + tabID, nil
	}
}
