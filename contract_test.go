package groups_test

import (
	"encoding/json"
	g "github.com/StuHelper/stuhelper-groups"
	"github.com/getkin/kin-openapi/openapi3"
	"testing"
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
	for name, value := range map[string]any{"Config": c, "Result": command(t, f, user, "apply", g.Command{Kind: "apply"})} {
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
	page, e := f.s.List(ctx, manager, "g", "members", "", 10)
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
	if e = doc.Components.Schemas["Page"].Value.VisitJSON(data); e != nil {
		t.Fatal(e)
	}
}
