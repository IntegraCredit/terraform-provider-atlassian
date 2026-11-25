package jira

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestWorkflowConditionGroupValidator(t *testing.T) {
	listType := types.ListType{ElemType: types.StringType}
	attributeTypes := map[string]attr.Type{
		"operation": types.StringType,
		"rules":     listType,
		"groups":    listType,
	}
	group := func(rules, groups types.List) types.Object {
		return types.ObjectValueMust(attributeTypes, map[string]attr.Value{
			"operation": types.StringValue("ALL"),
			"rules":     rules,
			"groups":    groups,
		})
	}
	null := types.ListNull(types.StringType)
	empty := types.ListValueMust(types.StringType, []attr.Value{})
	nonempty := types.ListValueMust(types.StringType, []attr.Value{types.StringValue("item")})
	unknown := types.ListUnknown(types.StringType)

	for _, tc := range []struct {
		name    string
		value   types.Object
		wantErr bool
	}{
		{"omitted conditions", types.ObjectNull(attributeTypes), false},
		{"unknown conditions", types.ObjectUnknown(attributeTypes), false},
		{"omitted lists", group(null, null), true},
		{"empty lists", group(empty, empty), true},
		{"empty and omitted lists", group(empty, null), true},
		{"rule", group(nonempty, null), false},
		{"nested group", group(null, nonempty), false},
		{"unknown rules", group(unknown, empty), false},
		{"unknown groups", group(empty, unknown), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &validator.ObjectResponse{}
			workflowConditionGroupValidator{}.ValidateObject(context.Background(), validator.ObjectRequest{
				Path:        path.Root("conditions"),
				ConfigValue: tc.value,
			}, resp)
			if resp.Diagnostics.HasError() != tc.wantErr {
				t.Fatalf("diagnostics = %v, want error %t", resp.Diagnostics, tc.wantErr)
			}
		})
	}
}
