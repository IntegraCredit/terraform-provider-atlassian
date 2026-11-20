package jira_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

// TestIntegration_preflight verifies API access and admin permissions before the suite runs.
// If this test fails, all other integration tests will also fail — check credentials.
func TestIntegration_preflight(t *testing.T) {
	testutil.SkipIfNoAcc(t)

	client, err := testutil.SweepClient()
	if err != nil {
		t.Fatalf("preflight: cannot create client: %s", err)
	}

	ctx := context.Background()

	// Verify we can list projects (requires project-level read access).
	var result interface{}
	statusCode, err := client.GetWithStatus(ctx, "/rest/api/3/project/search?maxResults=1", &result)
	if err != nil {
		t.Fatalf("preflight: API access failed (status %d): %s", statusCode, err)
	}

	// Verify we can list groups (requires admin-level access).
	var groupResult interface{}
	statusCode, err = client.GetWithStatus(ctx, "/rest/api/3/group/bulk?maxResults=1", &groupResult)
	if err != nil {
		t.Fatalf("preflight: admin access failed (status %d) — check that the API token belongs to a Jira admin: %s", statusCode, err)
	}

	// Listing groups does not need Jira admin, but schemes, workflows and
	// custom fields do. Check the global permission itself.
	var perms struct {
		Permissions map[string]struct {
			HavePermission bool `json:"havePermission"`
		} `json:"permissions"`
	}
	if err := client.Get(ctx, "/rest/api/3/mypermissions?permissions=ADMINISTER", &perms); err != nil {
		t.Fatalf("preflight: checking the Jira admin permission failed: %s", err)
	}
	if !perms.Permissions["ADMINISTER"].HavePermission {
		t.Errorf("preflight: the account is not a Jira administrator — add it to the site's jira-admins group")
	}

	// Jira refuses to delete the last standard issue type. Without a permanent
	// one, tests that delete their issue type fail on cleanup, depending on
	// which test runs last.
	var issueTypes []preflightIssueType
	if err := client.Get(ctx, "/rest/api/3/issuetype", &issueTypes); err != nil {
		t.Fatalf("preflight: listing issue types failed: %s", err)
	}
	if !hasPermanentStandardIssueType(issueTypes) {
		t.Errorf("preflight: the site has no permanent standard issue type — create one, such as Task, outside the tests")
	}
}

type preflightIssueType struct {
	Name           string      `json:"name"`
	HierarchyLevel int64       `json:"hierarchyLevel"`
	Scope          interface{} `json:"scope"`
}

// hasPermanentStandardIssueType reports whether a global, base-level issue
// type exists that the tests did not create. Team-managed issue types carry a
// project scope and do not count.
func hasPermanentStandardIssueType(issueTypes []preflightIssueType) bool {
	for _, it := range issueTypes {
		if it.HierarchyLevel == 0 && it.Scope == nil && !strings.HasPrefix(it.Name, "tf-acc-test-") {
			return true
		}
	}
	return false
}
