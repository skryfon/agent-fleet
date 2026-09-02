package budget

import "testing"

func TestCheck(t *testing.T) {
	cases := []struct {
		name    string
		caps    Caps
		spent   Spent
		wantNil bool
		wantKnd string
	}{
		{"under every cap", Caps{USD: 8, Minutes: 45, Questions: 3}, Spent{USD: 1, Minutes: 1, Questions: 1}, true, ""},
		{"usd breach", Caps{USD: 8, Minutes: 45, Questions: 3}, Spent{USD: 8.01, Minutes: 1, Questions: 1}, false, "usd"},
		{"minutes breach", Caps{USD: 8, Minutes: 45, Questions: 3}, Spent{USD: 1, Minutes: 46, Questions: 1}, false, "minutes"},
		{"questions breach", Caps{USD: 8, Minutes: 45, Questions: 3}, Spent{USD: 1, Minutes: 1, Questions: 4}, false, "questions"},
		{"usd checked first when several breach", Caps{USD: 8, Minutes: 45, Questions: 3}, Spent{USD: 9, Minutes: 99, Questions: 9}, false, "usd"},
		{"zero cap is uncapped", Caps{}, Spent{USD: 1000, Minutes: 1000, Questions: 1000}, true, ""},
		{"exactly at cap does not breach", Caps{USD: 8}, Spent{USD: 8}, true, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Check(c.caps, c.spent)
			if c.wantNil {
				if got != nil {
					t.Fatalf("Check() = %v, want nil", got)
				}

				return
			}

			if got == nil {
				t.Fatalf("Check() = nil, want a breach")
			}

			if got.Kind != c.wantKnd {
				t.Fatalf("Check().Kind = %q, want %q", got.Kind, c.wantKnd)
			}
		})
	}
}
