package groups_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	g "github.com/StuHelper/stuhelper-groups"
	"github.com/StuHelper/stuhelper-groups/httpapi"
	"github.com/StuHelper/stuhelper-groups/sdk"
	"github.com/StuHelper/stuhelper-platform/account"
	"github.com/getkin/kin-openapi/openapi3"
)

func TestOpenAPIAndGeneratedResponseShapes(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, e := loader.LoadFromData(g.OpenAPI())
	if e != nil {
		t.Fatal(e)
	}
	if e = doc.Validate(ctx); e != nil {
		t.Fatal(e)
	}
	f := setup(t)
	c, e := f.s.Configuration(ctx, manager, "g")
	if e != nil {
		t.Fatal(e)
	}
	for name, value := range map[string]any{"GroupConfig": c, "Result": command(t, f, user, "apply", g.Command{Kind: "apply"})} {
		b, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		var data any
		if e = json.Unmarshal(b, &data); e != nil {
			t.Fatal(e)
		}
		if e = doc.Components.Schemas[name].Value.VisitJSON(data); e != nil {
			t.Fatalf("%s: %v", name, e)
		}
	}
	page, e := f.s.Members(ctx, manager, "g", "", 10)
	if e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal(page)
	if e != nil {
		t.Fatal(e)
	}
	var data any
	if e = json.Unmarshal(b, &data); e != nil {
		t.Fatal(e)
	}
	if e = doc.Components.Schemas["MemberPage"].Value.VisitJSON(data); e != nil {
		t.Fatal(e)
	}
}

func TestAPIPrefixMatchesContractRoutesAndGeneratedClient(t *testing.T) {
	doc, e := openapi3.NewLoader().LoadFromData(g.OpenAPI())
	if e != nil {
		t.Fatal(e)
	}
	if doc.Info.Version != "0.4.0" {
		t.Fatal(doc.Info.Version)
	}
	handler := httpapi.Handler(&g.Service{}, nil)
	for path, item := range doc.Paths.Map() {
		if path != "/api/v1/groups" && !strings.HasPrefix(path, "/api/v1/groups/") {
			t.Fatalf("unexpected path: %s", path)
		}
		concrete := strings.NewReplacer("{group}", "g", "{collection}", "members").Replace(path)
		for method := range item.Operations() {
			for _, prefix := range []string{"", "legacy"} {
				target := concrete
				want := http.StatusUnauthorized
				if prefix == "legacy" {
					target = strings.TrimPrefix(concrete, "/api")
					want = http.StatusNotFound
				}
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
				if recorder.Code != want {
					t.Fatalf("%s %s returned %d, want %d", method, target, recorder.Code, want)
				}
			}
		}
	}
	f := setup(t)
	server := httptest.NewServer(httpapi.Handler(f.s, func(*http.Request) (account.Principal, error) { return manager, nil }))
	defer server.Close()
	client, e := sdk.NewClientWithResponses(server.URL)
	if e != nil {
		t.Fatal(e)
	}
	response, e := client.GetGroupWithResponse(ctx, "g")
	if e != nil {
		t.Fatal(e)
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Id != "g" {
		t.Fatalf("generated client did not reach group: %+v", response)
	}
}
