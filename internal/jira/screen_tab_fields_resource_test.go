package jira_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/jira"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

func testAccScreenTabFieldsConfig(fields ...string) string {
	quoted := make([]string, len(fields))
	for i, f := range fields {
		quoted[i] = fmt.Sprintf("%q", f)
	}
	return fmt.Sprintf(`
resource "atlassian_jira_screen_tab_fields" "t" {
  screen_id = "10100"
  tab_id    = "10200"
  fields    = [%s]
}
`, strings.Join(quoted, ", "))
}

// TestAccScreenTabFieldsResource_basic takes over a tab, reorders it in place,
// imports it, detects an out-of-band reorder, and empties the tab on destroy.
func TestAccScreenTabFieldsResource_basic(t *testing.T) {
	fake := jira.NewFakeScreenTabs(t)
	fake.SetFields("10100", "10200", "summary", "assignee")

	t.Setenv("ATLASSIAN_URL", fake.URL())
	t.Setenv("ATLASSIAN_USER", "test@test.com")
	t.Setenv("ATLASSIAN_TOKEN", "test-token")

	const addr = "atlassian_jira_screen_tab_fields.t"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		CheckDestroy: func(_ *terraform.State) error {
			if got := fake.Fields("10100", "10200"); len(got) != 0 {
				return fmt.Errorf("tab still holds %q after destroy", got)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: testAccScreenTabFieldsConfig("summary", "description", "labels"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "id", "10100/10200"),
					resource.TestCheckResourceAttr(addr, "fields.#", "3"),
					resource.TestCheckResourceAttr(addr, "fields.0", "summary"),
					resource.TestCheckResourceAttr(addr, "fields.1", "description"),
					resource.TestCheckResourceAttr(addr, "fields.2", "labels"),
					testAccCheckFakeTab(fake, "summary", "description", "labels"),
				),
			},
			{
				Config: testAccScreenTabFieldsConfig("labels", "summary", "description"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "fields.0", "labels"),
					testAccCheckFakeTab(fake, "labels", "summary", "description"),
				),
			},
			{
				ResourceName:      addr,
				ImportState:       true,
				ImportStateId:     "10100/10200",
				ImportStateVerify: true,
			},
			{
				// Someone reorders the tab in Jira: the next plan puts it back.
				PreConfig: func() {
					fake.SetFields("10100", "10200", "summary", "labels", "description")
				},
				Config:             testAccScreenTabFieldsConfig("labels", "summary", "description"),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: testAccScreenTabFieldsConfig("labels", "summary", "description"),
				Check:  testAccCheckFakeTab(fake, "labels", "summary", "description"),
			},
		},
	})
}

func testAccCheckFakeTab(fake *jira.FakeScreenTabs, want ...string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		if got := fake.Fields("10100", "10200"); !slices.Equal(got, want) {
			return fmt.Errorf("tab fields = %q, want %q", got, want)
		}
		return nil
	}
}
