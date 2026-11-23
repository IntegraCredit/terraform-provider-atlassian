package jira_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

// projectSchemeAssignmentCase describes one resource that assigns a scheme to a
// project through PUT {path} and reads it back through GET {path}?projectId=.
type projectSchemeAssignmentCase struct {
	resourceType string
	schemeAttr   string
	path         string
	bodyKey      string
	entryKey     string
	numericID    bool
	defaultID    string
}

var projectSchemeAssignmentCases = []projectSchemeAssignmentCase{
	{
		resourceType: "atlassian_jira_project_issue_type_scheme",
		schemeAttr:   "issue_type_scheme_id",
		path:         "/rest/api/3/issuetypescheme/project",
		bodyKey:      "issueTypeSchemeId",
		entryKey:     "issueTypeScheme",
		defaultID:    "10000",
	},
	{
		resourceType: "atlassian_jira_project_issue_type_screen_scheme",
		schemeAttr:   "issue_type_screen_scheme_id",
		path:         "/rest/api/3/issuetypescreenscheme/project",
		bodyKey:      "issueTypeScreenSchemeId",
		entryKey:     "issueTypeScreenScheme",
		defaultID:    "1",
	},
	{
		resourceType: "atlassian_jira_project_workflow_scheme",
		schemeAttr:   "workflow_scheme_id",
		path:         "/rest/api/3/workflowscheme/project",
		bodyKey:      "workflowSchemeId",
		entryKey:     "workflowScheme",
		numericID:    true,
		defaultID:    "10000",
	},
}

// projectSchemeAssignmentMock is a fake Jira that holds one project's scheme and
// records every scheme the provider assigns to it, in order.
type projectSchemeAssignmentMock struct {
	mu       sync.Mutex
	current  string
	assigned []string
}

func (m *projectSchemeAssignmentMock) setCurrent(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.current = id
}

func (m *projectSchemeAssignmentMock) history() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.assigned)
}

func (m *projectSchemeAssignmentMock) server(t *testing.T, tc projectSchemeAssignmentCase, projectID string) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path != tc.path {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		switch r.Method {
		case http.MethodPut:
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if body["projectId"] != projectID || body[tc.bodyKey] == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			m.mu.Lock()
			m.current = body[tc.bodyKey]
			m.assigned = append(m.assigned, body[tc.bodyKey])
			m.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		case http.MethodGet:
			if r.URL.Query().Get("projectId") != projectID {
				json.NewEncoder(w).Encode(pageResponse([]interface{}{})) //nolint:errcheck
				return
			}
			m.mu.Lock()
			current := m.current
			m.mu.Unlock()

			var id interface{} = current
			if tc.numericID {
				n, err := strconv.Atoi(current)
				if err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				id = n
			}
			json.NewEncoder(w).Encode(pageResponse([]interface{}{ //nolint:errcheck
				map[string]interface{}{
					tc.entryKey:  map[string]interface{}{"id": id, "name": "Test Scheme"},
					"projectIds": []string{projectID},
				},
			}))

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

func projectSchemeAssignmentConfig(tc projectSchemeAssignmentCase, projectID, schemeID string) string {
	return fmt.Sprintf(`resource %q "test" {
  project_id = %q
  %s = %q
}`, tc.resourceType, projectID, tc.schemeAttr, schemeID)
}

func setMockAtlassianEnv(t *testing.T, url string) {
	t.Helper()
	t.Setenv("ATLASSIAN_URL", url)
	t.Setenv("ATLASSIAN_USER", "test@test.com")
	t.Setenv("ATLASSIAN_TOKEN", "test-token")
}

// A scheme change must be one in-place assignment. A replace would move the
// project to the default scheme first, which changes a live project's screens,
// issue types or workflows until the new scheme is assigned.
func TestAccProjectSchemeAssignment_updateInPlace(t *testing.T) {
	for _, tc := range projectSchemeAssignmentCases {
		t.Run(tc.resourceType, func(t *testing.T) {
			projectID := "10400"
			mock := &projectSchemeAssignmentMock{}
			srv := mock.server(t, tc, projectID)
			defer srv.Close()
			setMockAtlassianEnv(t, srv.URL)

			address := tc.resourceType + ".test"

			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
				CheckDestroy: func(_ *terraform.State) error {
					want := []string{"20001", "20002", tc.defaultID}
					if got := mock.history(); !slices.Equal(got, want) {
						return fmt.Errorf("assigned schemes %v, want %v", got, want)
					}
					return nil
				},
				Steps: []resource.TestStep{
					{
						Config: projectSchemeAssignmentConfig(tc, projectID, "20001"),
					},
					{
						Config: projectSchemeAssignmentConfig(tc, projectID, "20002"),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate),
							},
						},
						Check: resource.TestCheckResourceAttr(address, tc.schemeAttr, "20002"),
					},
				},
			})
		})
	}
}

// A scheme changed outside Terraform must show as an update back to the
// configured scheme, not as a missing resource to create again.
func TestAccProjectSchemeAssignment_driftIsUpdate(t *testing.T) {
	for _, tc := range projectSchemeAssignmentCases {
		t.Run(tc.resourceType, func(t *testing.T) {
			projectID := "10401"
			mock := &projectSchemeAssignmentMock{}
			srv := mock.server(t, tc, projectID)
			defer srv.Close()
			setMockAtlassianEnv(t, srv.URL)

			address := tc.resourceType + ".test"

			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
				CheckDestroy: func(_ *terraform.State) error {
					return nil
				},
				Steps: []resource.TestStep{
					{
						Config: projectSchemeAssignmentConfig(tc, projectID, "20001"),
					},
					{
						PreConfig: func() { mock.setCurrent("20003") },
						Config:    projectSchemeAssignmentConfig(tc, projectID, "20001"),
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{
								plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate),
							},
						},
						Check: resource.TestCheckResourceAttr(address, tc.schemeAttr, "20001"),
					},
				},
			})
		})
	}
}
