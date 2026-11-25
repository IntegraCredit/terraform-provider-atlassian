# A new workflow: its statuses, and an initial transition into the first one.
resource "atlassian_jira_workflow" "example" {
  name = "Example Workflow"
  statuses = [
    atlassian_jira_status.example.id,
  ]
}

# An existing workflow, imported with its transitions. A change, such as a new
# status with transitions into and out of it, is an in-place update. Keep the
# IDs of existing transitions, and give a new transition a new numeric ID.
import {
  to = atlassian_jira_workflow.development
  id = "Development Workflow" # the workflow's name or entity ID
}

resource "atlassian_jira_workflow" "development" {
  name     = "Development Workflow"
  statuses = ["10000", "3", "10001"]

  transitions = {
    "1" = {
      name = "Create"
      type = "INITIAL"
      to   = "10000"
      validators = [{
        rule_key   = "system:check-permission-validator"
        parameters = { permissionKey = "CREATE_ISSUES" }
      }]
    }

    "11" = {
      name = "Start progress"
      type = "DIRECTED"
      from = ["10000"]
      to   = "3"
      conditions = {
        operation = "ANY"
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
      }
    }

    "21" = {
      name = "Done"
      type = "DIRECTED"
      from = ["10000", "3"]
      to   = "10001"
      actions = [{
        rule_key   = "system:update-field"
        parameters = { field = "resolution", value = "10000", mode = "replace" }
      }]
    }

    "31" = {
      name = "Reopen"
      type = "GLOBAL"
      to   = "10000"
    }
  }
}
