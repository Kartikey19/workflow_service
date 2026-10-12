package schedules

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	valid := []struct{ cron, zone string }{
		{"0 9 * * 1-5", "Europe/London"},
		{"*/15 * * * *", ""},
		{"@daily", "Asia/Kolkata"},
		{"@every 5m", "UTC"},
	}
	for _, v := range valid {
		if _, _, err := Parse(v.cron, v.zone); err != nil {
			t.Errorf("Parse(%q, %q) = %v", v.cron, v.zone, err)
		}
	}
	invalid := []struct{ cron, zone string }{
		{"", "UTC"},
		{"61 * * * *", "UTC"},
		{"0 9 * * *", "Mars/Olympus"},
		{"0 9 * * *", "Local"},
		{"@every 10s", "UTC"},
		{"CRON_TZ=UTC 0 9 * * *", "UTC"},
		{"0 0 9 * * *", "UTC"}, // seconds field is not supported
	}
	for _, v := range invalid {
		if _, _, err := Parse(v.cron, v.zone); err == nil {
			t.Errorf("Parse(%q, %q) accepted an invalid schedule", v.cron, v.zone)
		}
	}
}

func TestPreviewFollowsTimezoneAndDST(t *testing.T) {
	s := &Service{now: func() time.Time { return time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC) }}
	times, err := s.Preview("0 9 * * *", "Europe/London", 3)
	if err != nil {
		t.Fatal(err)
	}
	// Britain moves to BST on 29 March 2026: 09:00 local is 09:00Z, then 08:00Z.
	want := []string{"2026-03-29T08:00:00Z", "2026-03-30T08:00:00Z", "2026-03-31T08:00:00Z"}
	if len(times) != len(want) {
		t.Fatalf("got %d times", len(times))
	}
	for i, w := range want {
		if got := times[i].UTC().Format(time.RFC3339); got != w {
			t.Errorf("time %d = %s, want %s", i, got, w)
		}
	}
}

func TestSlotFor(t *testing.T) {
	schedule, loc, err := Parse("0 * * * *", "UTC") // hourly
	if err != nil {
		t.Fatal(err)
	}
	due := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		now       time.Time
		catchUp   string
		slot      time.Time
		fire      bool
		nextAfter time.Time
	}{
		{"on time", due.Add(5 * time.Second), "skip", due, true, due.Add(time.Hour)},
		{"late within grace", due.Add(90 * time.Second), "skip", due, true, due.Add(time.Hour)},
		{"missed and skipped", due.Add(3*time.Hour + 10*time.Minute), "skip", due.Add(3 * time.Hour), false, due.Add(4 * time.Hour)},
		{"missed slots collapse into the latest", due.Add(3*time.Hour + 10*time.Minute), "latest", due.Add(3 * time.Hour), true, due.Add(4 * time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slot, fire, next := slotFor(schedule, loc, due, tc.catchUp, tc.now)
			if !slot.Equal(tc.slot) || fire != tc.fire || !next.Equal(tc.nextAfter) {
				t.Fatalf("slotFor = %s %v %s, want %s %v %s", slot, fire, next, tc.slot, tc.fire, tc.nextAfter)
			}
		})
	}
}
