package main

import (
	"testing"
	"time"
)

func TestExpandLink(t *testing.T) {
	tests := []struct {
		name      string
		long      string
		remainder string
		now       time.Time
		want      string
	}{
		{
			name: "dont-mangle-escapes",
			long: "http://host.com/foo%2f/bar",
			want: "http://host.com/foo%2f/bar",
		},
		{
			name:      "dont-mangle-escapes-and-remainder",
			long:      "http://host.com/foo%2f/bar",
			remainder: "extra",
			want:      "http://host.com/foo%2f/bar/extra",
		},
		{
			name:      "remainder-insert-slash",
			long:      "http://host.com/foo",
			remainder: "extra",
			want:      "http://host.com/foo/extra",
		},
		{
			name:      "remainder-long-as-trailing-slash",
			long:      "http://host.com/foo/",
			remainder: "extra",
			want:      "http://host.com/foo/extra",
		},
		{
			name: "var-expansions-time",
			long: "$https://roamresearch.com/#/app/ts-corp/page/${MM}-${DD}-${YYYY}",
			want: "https://roamresearch.com/#/app/ts-corp/page/06-02-2022",
			now:  time.Date(2022, 06, 02, 1, 2, 3, 4, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandLink(tt.long, tt.remainder, expandEnv{Now: tt.now})
			if err != nil {
				t.Fatalf("expandLink(%q): %v", tt.long, err)
			}
			if got != tt.want {
				t.Errorf("expandLink(%q) = %q; want %q", tt.long, got, tt.want)
			}
		})
	}
}
