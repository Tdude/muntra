package api

import "testing"

// Regression test for a real production incident: a consumer building a
// complete per-URL view-count map requested limit=5000 to cover every page
// on a site. A stale cap here silently downgraded that to the 100-item
// default, which drops every URL outside the top 100 by traffic from the
// response — read by the client as "zero views" for a real, nonzero page.
func TestParseBreakdownLimit(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"empty falls back to default", "", defaultBreakdownLimit},
		{"typical small value", "50", 50},
		{"the exact caller request that regressed", "5000", 5000},
		{"at the max bound", "10000", maxBreakdownLimit},
		{"above the max bound falls back to default", "10001", defaultBreakdownLimit},
		{"zero falls back to default", "0", defaultBreakdownLimit},
		{"negative falls back to default", "-5", defaultBreakdownLimit},
		{"non-numeric falls back to default", "abc", defaultBreakdownLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseBreakdownLimit(tc.raw); got != tc.want {
				t.Errorf("parseBreakdownLimit(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}
