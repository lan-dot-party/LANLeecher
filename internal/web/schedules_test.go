package web

import (
	"testing"
	"time"
)

func TestParseScheduleWindow(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("UTC+2", 2*3600)
	defer func() { time.Local = saved }()

	// 15 to 18 April inclusive, no times: 00:00 local on day one to 24:00 local on the last day.
	start, end, ok := parseScheduleWindow("2027-04-15", "", "2027-04-18", "")
	if !ok || !start.Equal(time.Date(2027, 4, 14, 22, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2027, 4, 18, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("whole days: %v – %v, %v", start, end, ok)
	}
	if got := formatScheduleWindow(start, end); got != "2027-04-15 – 2027-04-18" {
		t.Errorf("format whole days: %q", got)
	}
	start, end, ok = parseScheduleWindow("2027-04-15", "18:00", "2027-04-18", "14:00")
	if !ok || !start.Equal(time.Date(2027, 4, 15, 16, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2027, 4, 18, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("with times: %v – %v, %v", start, end, ok)
	}
	if got := formatScheduleWindow(start, end); got != "2027-04-15 18:00 – 2027-04-18 14:00" {
		t.Errorf("format with times: %q", got)
	}
	start, end, _ = parseScheduleWindow("2027-04-15", "", "2027-04-15", "")
	if got := formatScheduleWindow(start, end); got != "2027-04-15" {
		t.Errorf("format one day: %q", got)
	}
	for _, c := range [][4]string{
		{"2027-04-18", "", "2027-04-15", ""}, // end before start
		{"2027-04-15", "10:00", "2027-04-15", "09:00"},
		{"15.04.2027", "", "2027-04-18", ""},      // wrong format
		{"2027-04-15", "25:00", "2027-04-18", ""}, // bad time
		{"", "", "2027-04-18", ""},
	} {
		if _, _, ok := parseScheduleWindow(c[0], c[1], c[2], c[3]); ok {
			t.Errorf("accepted %v", c)
		}
	}
}
