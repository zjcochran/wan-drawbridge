// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

func TestISPDataLimits(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	backup, _, _ := e.st.SeeASN(ctx, 64510, "SKYLINK-AS64510", time.Now())
	c := e.admin()
	path := "/isps/" + itoa64(int64(backup)) + "/data-limits"
	resp, body := c.htmx(path, url.Values{"outage": {"5"}, "month": {" 0.25 "}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `value="5"`) || !strings.Contains(body, `value="0.25"`) {
		t.Fatalf("save: %d %s", resp.StatusCode, body)
	}
	rows, _ := e.st.ISPs(ctx, time.Now())
	for _, r := range rows {
		if r.ID == backup && (r.Limits.OutageBytes != 5e9 || r.Limits.MonthBytes != 250e6) {
			t.Errorf("saved = %+v", r.Limits)
		}
	}
	if evs, _ := e.st.Events(ctx, store.EventFilter{Type: "isp_data_limits"}); len(evs) != 1 || evs[0].Payload["outage_bytes"] != float64(5e9) {
		t.Errorf("events = %+v", evs)
	}
	for _, bad := range []string{"-1", "abc", "NaN", "2000000"} {
		if resp, _ := c.htmx(path, url.Values{"outage": {bad}}); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%q: %d", bad, resp.StatusCode)
		}
	}
	if resp, _ := c.htmx("/isps/999/data-limits", url.Values{"outage": {"1"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("unknown ISP: %d", resp.StatusCode)
	}
	if resp, body := c.htmx(path, url.Values{"outage": {""}, "month": {"0"}}); resp.StatusCode != http.StatusOK || strings.Contains(body, `value="0"`) {
		t.Errorf("off: %d", resp.StatusCode)
	}
	v := e.viewerClient()
	if resp, _ := v.htmx(path, url.Values{"outage": {"1"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer: %d", resp.StatusCode)
	}
	c.htmx(path, url.Values{"outage": {"5"}, "month": {"100"}})
	if _, page := v.get("/isps"); !strings.Contains(page, `<div class="line">5 GB per outage</div>`) || !strings.Contains(page, `<div class="line">100 GB per month</div>`) {
		t.Error("the viewer does not see the limits")
	}
	if _, page := c.get("/settings/notifications"); strings.Contains(page, "Data limits") {
		t.Error("the notifications tab still has global data limits")
	}
}

func TestPrimaryDataLimitForm(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p, _, _ := e.st.SeeASN(ctx, 64500, "EXAMPLENET-64500", time.Now())
	e.st.SetRole(ctx, p, store.RolePrimary, true)
	c := e.admin()
	if _, page := c.get("/isps"); strings.Contains(page, `/isps/`+itoa64(int64(p))+`/data-limits"`) {
		t.Error("a new primary is metered")
	}
	e.st.SetMetered(ctx, p, true)
	_, page := c.get("/isps")
	_, form, found := strings.Cut(page, `hx-post="/isps/`+itoa64(int64(p))+`/data-limits"`)
	form, _, _ = strings.Cut(form, "</form>")
	if !found {
		t.Fatal("the primary has no data limits form")
	}
	if strings.Contains(form, "GB per outage") || !strings.Contains(form, "GB per month") || !strings.Contains(form, `type="hidden" name="outage"`) {
		t.Errorf("the primary's form: %s", form)
	}
	e.st.SetMetered(ctx, p, false)
	if _, page := c.get("/isps"); strings.Contains(page, `/isps/`+itoa64(int64(p))+`/data-limits"`) {
		t.Error("an unmetered primary has data limits")
	}
}
