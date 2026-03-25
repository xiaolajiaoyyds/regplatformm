package worker

import "testing"

func TestNormalizeRegYYDSMailBaseURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty uses default", raw: "", want: defaultRegYYDSMailBaseURL},
		{name: "root url stays root", raw: "https://maliapi.215.im", want: defaultRegYYDSMailBaseURL},
		{name: "trailing slash trimmed", raw: "https://maliapi.215.im/", want: defaultRegYYDSMailBaseURL},
		{name: "v1 suffix stripped", raw: "https://maliapi.215.im/v1", want: defaultRegYYDSMailBaseURL},
		{name: "v1 suffix with slash stripped", raw: "https://maliapi.215.im/v1/", want: defaultRegYYDSMailBaseURL},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeRegYYDSMailBaseURL(tc.raw); got != tc.want {
				t.Fatalf("normalizeRegYYDSMailBaseURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
