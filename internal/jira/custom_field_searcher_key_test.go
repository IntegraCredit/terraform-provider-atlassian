package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

// These tests call the custom field Read and ImportState methods directly.
// They need no Terraform CLI and no TF_ACC, so they run in `make test` and CI.

const (
	searcherKeyFieldID   = "customfield_10200"
	searcherKeyFieldName = "tf-test-searcher-key"
	searcherKeyFieldType = "com.atlassian.jira.plugin.system.customfieldtypes:textfield"
	searcherKeySearcher  = "com.atlassian.jira.plugin.system.customfieldtypes:textsearcher"
)

// newSearcherKeyClient returns a client for a mock GET /rest/api/3/field/search.
// The mock rejects a search that does not expand searcherKey, because Jira
// returns searcherKey as null in that case.
func newSearcherKeyClient(t *testing.T) *atlassian.Client {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/rest/api/3/field/search" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		q := r.URL.Query()
		if !slices.Contains(strings.Split(q.Get("expand"), ","), "searcherKey") {
			http.Error(w, "expand=searcherKey is required", http.StatusBadRequest)
			return
		}

		values := []map[string]interface{}{}
		byID := q.Get("id") == searcherKeyFieldID
		byName := q.Get("query") == searcherKeyFieldName && q.Get("type") == "custom"
		if byID || byName {
			values = append(values, map[string]interface{}{
				"id":          searcherKeyFieldID,
				"name":        searcherKeyFieldName,
				"custom":      true,
				"description": "",
				"searcherKey": searcherKeySearcher,
				"schema": map[string]interface{}{
					"type":     "string",
					"custom":   searcherKeyFieldType,
					"customId": 10200,
				},
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"values": values}) //nolint:errcheck
	}))
	t.Cleanup(server.Close)

	client, err := atlassian.NewClient(atlassian.ClientConfig{
		URL:   server.URL,
		User:  "test@test.com",
		Token: "test-token",
	})
	if err != nil {
		t.Fatalf("creating client: %s", err)
	}
	return client
}

// TestCustomFieldResourceRead_KeepsSearcherKey reads a field whose state holds
// the configured searcher key. Read must keep it, or the next plan replaces the field.
func TestCustomFieldResourceRead_KeepsSearcherKey(t *testing.T) {
	ctx := context.Background()
	r := &customFieldResource{client: newSearcherKeyClient(t)}

	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)

	state := tfsdk.State{
		Schema: schemaResp.Schema,
		Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
	}
	diags := state.Set(ctx, &customFieldResourceModel{
		ID:          types.StringValue(searcherKeyFieldID),
		Name:        types.StringValue(searcherKeyFieldName),
		Description: types.StringValue(""),
		Type:        types.StringValue(searcherKeyFieldType),
		SearcherKey: types.StringValue(searcherKeySearcher),
	})
	if diags.HasError() {
		t.Fatalf("building state: %v", diags)
	}

	resp := resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}

	var got customFieldResourceModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("reading state: %v", diags)
	}
	if got.SearcherKey.ValueString() != searcherKeySearcher {
		t.Errorf("searcher_key = %q, want %q", got.SearcherKey.ValueString(), searcherKeySearcher)
	}
}

// TestCustomFieldResourceImportState_SetsSearcherKey imports a field by ID.
// An empty searcher_key here would replace every imported field that sets one.
func TestCustomFieldResourceImportState_SetsSearcherKey(t *testing.T) {
	ctx := context.Background()
	r := &customFieldResource{client: newSearcherKeyClient(t)}

	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)

	resp := resource.ImportStateResponse{
		State: tfsdk.State{
			Schema: schemaResp.Schema,
			Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
		},
	}
	r.ImportState(ctx, resource.ImportStateRequest{ID: searcherKeyFieldID}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ImportState: %v", resp.Diagnostics)
	}

	var got types.String
	if diags := resp.State.GetAttribute(ctx, path.Root("searcher_key"), &got); diags.HasError() {
		t.Fatalf("reading searcher_key: %v", diags)
	}
	if got.ValueString() != searcherKeySearcher {
		t.Errorf("searcher_key = %q, want %q", got.ValueString(), searcherKeySearcher)
	}
}

// TestCustomFieldDataSourceRead_SetsSearcherKey looks up a field by name.
func TestCustomFieldDataSourceRead_SetsSearcherKey(t *testing.T) {
	ctx := context.Background()
	d := &customFieldDataSource{client: newSearcherKeyClient(t)}

	var schemaResp datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &schemaResp)
	objectType := schemaResp.Schema.Type().TerraformType(ctx)

	config := tfsdk.Config{
		Schema: schemaResp.Schema,
		Raw: tftypes.NewValue(objectType, map[string]tftypes.Value{
			"name":         tftypes.NewValue(tftypes.String, searcherKeyFieldName),
			"id":           tftypes.NewValue(tftypes.String, nil),
			"description":  tftypes.NewValue(tftypes.String, nil),
			"type":         tftypes.NewValue(tftypes.String, nil),
			"searcher_key": tftypes.NewValue(tftypes.String, nil),
		}),
	}
	resp := datasource.ReadResponse{
		State: tfsdk.State{Schema: schemaResp.Schema, Raw: tftypes.NewValue(objectType, nil)},
	}
	d.Read(ctx, datasource.ReadRequest{Config: config}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}

	var got types.String
	if diags := resp.State.GetAttribute(ctx, path.Root("searcher_key"), &got); diags.HasError() {
		t.Fatalf("reading searcher_key: %v", diags)
	}
	if got.ValueString() != searcherKeySearcher {
		t.Errorf("searcher_key = %q, want %q", got.ValueString(), searcherKeySearcher)
	}
}
