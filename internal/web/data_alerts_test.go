// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

func TestDataAlertsSettings(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	if _, page := c.get("/settings/notifications"); !strings.Contains(page, "Save data limits") {
		t.Fatal("the notifications tab has no data limits")
	}
	resp, body := c.htmx("/settings/data-alerts", url.Values{"outage": {"5"}, "month": {" 0.25 "}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `value="5"`) || !strings.Contains(body, `value="0.25"`) {
		t.Fatalf("save: %d %s", resp.StatusCode, body)
	}
	var a store.DataAlerts
	e.st.GetSetting(context.Background(), store.SettingDataAlerts, &a)
	if a.OutageBytes != 5e9 || a.MonthBytes != 250e6 {
		t.Errorf("saved = %+v", a)
	}
	for _, bad := range []string{"-1", "abc", "NaN", "2000000"} {
		if resp, _ := c.htmx("/settings/data-alerts", url.Values{"outage": {bad}}); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%q: %d", bad, resp.StatusCode)
		}
	}
	if resp, body := c.htmx("/settings/data-alerts", url.Values{"outage": {""}, "month": {"0"}}); resp.StatusCode != http.StatusOK || strings.Contains(body, `value="0"`) {
		t.Errorf("off: %d", resp.StatusCode)
	}
	if resp, _ := e.viewerClient().htmx("/settings/data-alerts", url.Values{"outage": {"1"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer: %d", resp.StatusCode)
	}
}
