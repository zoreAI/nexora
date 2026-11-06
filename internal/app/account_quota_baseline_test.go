package app

import "testing"

func TestQuotaRefreshRebasesReferencePercentage(t *testing.T) {
	window := "2026-11-07T00:00:00Z"
	nextWindow := "2026-11-08T00:00:00Z"
	store := NewStore(t.TempDir())
	defer store.Close()
	if err := store.SaveAccounts([]Account{{AccessToken: "fixture", Status: "正常", Quota: 5, InitialQuota: 25, RestoreAt: &window}}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store}
	for _, tc := range []struct {
		name     string
		quota    int
		reset    *string
		unknown  bool
		baseline int
	}{
		{"legacy historical 25 is replaced", 5, &window, false, 5},
		{"usage in same window retains reference", 3, &window, false, 5},
		{"larger upstream allowance raises reference", 7, &window, false, 7},
		{"new window discards old reference", 4, &nextWindow, false, 4},
		{"exhausted retains reference", 0, &nextWindow, false, 4},
		{"unknown clears reference", 0, nil, true, -1},
		{"quota resumes", 5, &nextWindow, false, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := store.LoadAccounts()[0]
			info := Account{Status: "正常", Quota: tc.quota, RestoreAt: tc.reset, RateLimitResetAt: tc.reset, ImageQuotaUnknown: tc.unknown, QuotaCheckedAt: nowISO()}
			if tc.quota == 0 && !tc.unknown {
				info.Status = "限流"
			}
			if err := s.applyAccountInfoRefresh(observed, info); err != nil {
				t.Fatal(err)
			}
			got := store.LoadAccounts()[0]
			if tc.baseline < 0 {
				if got.ImageQuotaBaseline != nil {
					t.Fatal("unknown quota retained a percentage baseline")
				}
			} else if got.ImageQuotaBaseline == nil || *got.ImageQuotaBaseline != tc.baseline {
				t.Fatalf("reference baseline = %v, want %d", got.ImageQuotaBaseline, tc.baseline)
			}
			if got.InitialQuota != 25 {
				t.Fatal("legacy historical peak was overwritten")
			}
		})
	}
}

func TestQuotaReferenceComparesResetInstants(t *testing.T) {
	previous := "2026-11-07T08:00:00+08:00"
	current := "2026-11-07T00:00:00Z"
	baseline := 5
	got := refreshedImageQuotaBaseline(Account{RestoreAt: &previous, ImageQuotaBaseline: &baseline}, Account{Quota: 3, RestoreAt: &current})
	if got == nil || *got != 5 {
		t.Fatal("equivalent reset timestamps started a new reference window")
	}
}
