// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// maxAlertGB caps a data limit: 1 PB.
const maxAlertGB = 1e6

// dataAlertsForm shows the data limits in GB; "" is off.
type dataAlertsForm struct {
	Outage, Month string
}

func gbText(b int64) string {
	if b <= 0 {
		return ""
	}
	return strconv.FormatFloat(float64(b)/1e9, 'f', -1, 64)
}

// parseGB reads a limit in GB; blank and 0 turn it off.
func parseGB(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	gb, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(gb) || gb < 0 || gb > maxAlertGB {
		return 0, errors.New("a limit is a number of GB from 0 to 1000000")
	}
	return int64(math.Round(gb * 1e9)), nil
}

func (s *Server) dataAlertsForm(ctx context.Context) (dataAlertsForm, error) {
	var a store.DataAlerts
	_, err := s.Store.GetSetting(ctx, store.SettingDataAlerts, &a)
	return dataAlertsForm{Outage: gbText(a.OutageBytes), Month: gbText(a.MonthBytes)}, err
}

func (s *Server) saveDataAlerts(w http.ResponseWriter, r *http.Request) {
	var a store.DataAlerts
	var err error
	if a.OutageBytes, err = parseGB(r.PostFormValue("outage")); err == nil {
		a.MonthBytes, err = parseGB(r.PostFormValue("month"))
	}
	if err != nil {
		invalid(w, err.Error())
		return
	}
	if err := s.Store.SetSetting(r.Context(), store.SettingDataAlerts, a); err != nil {
		s.fail(w, r, err)
		return
	}
	s.event(r.Context(), "settings_changed", map[string]any{"by": actorName(r)})
	w.Header().Set("HX-Trigger", `{"toast":"Data limits saved."}`)
	s.partial(w, r, "settings", "data-alerts", dataAlertsForm{Outage: gbText(a.OutageBytes), Month: gbText(a.MonthBytes)})
}
