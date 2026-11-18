package groups_test

import (
	"context"
	"encoding/json"
	g "github.com/StuHelper/stuhelper-groups"
	"testing"
)

type analyzeFunc func(context.Context, string) (string, []string, error)

func (f analyzeFunc) Analyze(c context.Context, input string) (string, []string, error) {
	return f(c, input)
}
func TestAnalysisInputFields(t *testing.T) {
	f := setup(t)
	f.s.AI = analyzeFunc(func(_ context.Context, input string) (string, []string, error) {
		var fields map[string]string
		if err := json.Unmarshal([]byte(input), &fields); err != nil || len(fields) != 2 || fields["body"] != "message" || fields["reason"] != "reason" {
			t.Fatal("unexpected analysis fields")
		}
		return "ok", nil, nil
	})
	if err := f.s.Ingest(ctx, "g", event("m", "qq-bot", "g", "groups.message", 1), g.Message{ID: "m", Owner: user.Key(), Body: "message"}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.MemberEvent(ctx, "g", event("reporter-joined", "qq-bot", "g", "groups.member_joined", 1), user.Key()); err != nil {
		t.Fatal(err)
	}
	r := command(t, f, user, "report", g.Command{Kind: "report", Message: "m", Text: "reason"})
	command(t, f, manager, "analysis", g.Command{Kind: "analyze", Target: r.ID})
}
