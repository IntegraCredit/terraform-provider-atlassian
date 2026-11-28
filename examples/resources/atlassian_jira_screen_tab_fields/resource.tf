# Owns every field on the tab, in this order. Fields not listed are removed.
# Do not also manage this tab with atlassian_jira_screen_tab_field.
resource "atlassian_jira_screen_tab_fields" "example" {
  screen_id = atlassian_jira_screen.example.id
  # The tab Jira created with the screen. For a tab you added, use
  # atlassian_jira_screen_tab.example.id.
  tab_id = atlassian_jira_screen.example.default_tab_id

  fields = [
    "summary",
    "issuetype",
    "description",
    atlassian_jira_custom_field.example.id,
    "labels",
  ]

  # Destroying this resource removes the listed fields from the tab.
  lifecycle {
    prevent_destroy = true
  }
}

# To stop managing a tab and keep its fields, remove the resource with
# destroy = false instead of deleting the block:
#
# removed {
#   from = atlassian_jira_screen_tab_fields.example
#   lifecycle {
#     destroy = false
#   }
# }
