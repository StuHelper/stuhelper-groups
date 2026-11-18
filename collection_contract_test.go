package groups_test

import (
	"context"
	"encoding/json"
	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-groups/sdk"
	"github.com/getkin/kin-openapi/openapi3"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCollectionUnknownQuery(t *testing.T) {
	f := setup(t)
	for _, path := range []string{"", "/g/members", "/g/actions", "/g/watch", "/g/messages", "/g/reports", "/g/blacklist", "/g/subscriptions", "/g/deliveries", "/g/warnings", "/g/hits", "/me"} {
		w := api(t, f, manager, "GET", "/api/v1/groups"+path+"?unknown=value", "", nil)
		if w.Code != 400 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
}

func TestDeliverySDKCompatibility(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/groups/g/deliveries" {
			t.Errorf("path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"next_cursor":""}`))
	}))
	defer server.Close()
	client, err := sdk.NewClientWithResponses(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range []func(context.Context, string, *sdk.ListDeliveriesParams, ...sdk.RequestEditorFn) (*sdk.ListDeliveriesResponse, error){client.ListDeliverysWithResponse, client.ListDeliveriesWithResponse} {
		page, err := list(ctx, "g", nil)
		if err != nil || page.JSON200 == nil {
			t.Fatal(page, err)
		}
	}
}

func TestConfigurationContractRejectsInvalidSettings(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(g.OpenAPI())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(config())
	if err != nil {
		t.Fatal(err)
	}
	configSchema := doc.Components.Schemas["GroupConfigInput"].Value
	for field, bad := range map[string][]any{"id": {"*", "a/b", "me"}, "school": {"*", "a/b"}, "dice_sides": {float64(1)}} {
		for _, value := range bad {
			var input map[string]any
			if err := json.Unmarshal(raw, &input); err != nil {
				t.Fatal(err)
			}
			if err := configSchema.VisitJSON(input); err != nil {
				t.Fatal(err)
			}
			input[field] = value
			if err := configSchema.VisitJSON(input); err == nil {
				t.Fatalf("accepted %s=%v", field, value)
			}
		}
	}
	seconds := doc.Components.Schemas["BlacklistInput"].Value.Properties["seconds"].Value
	if seconds.VisitJSON(float64(31536000)) != nil || seconds.VisitJSON(float64(31536001)) == nil {
		t.Fatal("blacklist seconds boundary")
	}
}
