// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/zjcochran/wan-drawbridge/internal/store"
)

// maxLimitGB caps a data limit: 1 PB.
const maxLimitGB = 1e6

// gbText shows a data limit in GB; "" is off.
func gbText(b int64) string {
	if b <= 0 {
		return ""
	}
	return strconv.FormatFloat(float64(b)/1e9, 'f', -1, 64)
}

// parseGB reads a data limit in GB; blank and 0 turn it off.
func parseGB(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	gb, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(gb) || gb < 0 || gb > maxLimitGB {
		return 0, errors.New("a data limit is a number of GB from 0 to 1000000")
	}
	return int64(math.Round(gb * 1e9)), nil
}

func (s *Server) setISPDataLimits(w http.ResponseWriter, r *http.Request) {
	id := atoi(r.PathValue("id"), 0)
	var l store.DataLimits
	var err error
	if l.OutageBytes, err = parseGB(r.PostFormValue("outage")); err == nil {
		l.MonthBytes, err = parseGB(r.PostFormValue("month"))
	}
	if err == nil {
		err = s.Store.SetDataLimits(r.Context(), id, l)
	}
	if err != nil {
		invalid(w, err.Error())
		return
	}
	s.event(r.Context(), "isp_data_limits", map[string]any{"isp": id, "outage_bytes": l.OutageBytes, "month_bytes": l.MonthBytes, "by": actorName(r)})
	w.Header().Set("HX-Trigger", `{"toast":"Data limits saved."}`)
	s.ispsTable(w, r)
}
