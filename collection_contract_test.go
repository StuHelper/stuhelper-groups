package groups_test

import (
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
