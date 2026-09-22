package api_test

import (
	"testing"

	"github.com/open-rails/helpers/api"
)

func TestListParamsNormalize(t *testing.T) {
	cases := []struct {
		name                   string
		in                     api.ListParams
		defaultLimit, maxLimit int
		wantLimit, wantOffset  int
	}{
		{"zero limit takes the default", api.ListParams{}, 20, 100, 20, 0},
		{"negative limit takes the default", api.ListParams{Limit: -5}, 20, 100, 20, 0},
		{"limit is capped", api.ListParams{Limit: 5000}, 20, 100, 100, 0},
		{"negative offset floors at zero", api.ListParams{Limit: 10, Offset: -3}, 20, 100, 10, 0},
		{"a valid page is untouched", api.ListParams{Limit: 50, Offset: 100}, 20, 100, 50, 100},
		{"a zero cap means uncapped", api.ListParams{Limit: 5000}, 20, 0, 5000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.in
			p.Normalize(tc.defaultLimit, tc.maxLimit)
			if p.Limit != tc.wantLimit || p.Offset != tc.wantOffset {
				t.Errorf("got limit=%d offset=%d, want limit=%d offset=%d", p.Limit, p.Offset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

func TestHasMore(t *testing.T) {
	if !api.HasMore(0, 20, 21) {
		t.Error("21 rows past a first page of 20 has more")
	}
	if api.HasMore(0, 20, 20) {
		t.Error("exactly one page has no more")
	}
	if api.HasMore(20, 20, 20) {
		t.Error("past the end has no more")
	}
}

func TestHasMoreFromLen(t *testing.T) {
	if !api.HasMoreFromLen(0, 20, 21) {
		t.Error("want more")
	}
	if api.HasMoreFromLen(10, 5, 15) {
		t.Error("a short page reaching the total has no more")
	}
}
