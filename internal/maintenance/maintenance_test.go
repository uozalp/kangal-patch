package maintenance

import (
	"testing"
	"time"

	"github.com/uozalp/kangal-patch/api/v1alpha1"
)

func TestIsInMaintenanceWindow(t *testing.T) {
	night := v1alpha1.MaintenanceWindow{Days: []string{"Sun"}, StartTime: "23:00", EndTime: "01:00"}
	tests := []struct {
		name string
		spec *v1alpha1.MaintenanceSpec
		at   string
		want bool
	}{
		{"nil spec always allows", nil, "2026-10-12T12:00:00Z", true},
		{"no windows always allows", &v1alpha1.MaintenanceSpec{}, "2026-10-12T12:00:00Z", true},
		{"excluded date blocks", &v1alpha1.MaintenanceSpec{ExcludeDates: []string{"2026-10-12"}, Windows: []v1alpha1.MaintenanceWindow{{StartTime: "00:00", EndTime: "23:59"}}}, "2026-10-12T12:00:00Z", false},
		{"disabled window is skipped", &v1alpha1.MaintenanceSpec{Windows: []v1alpha1.MaintenanceWindow{{StartTime: "00:00", EndTime: "23:59", Disabled: true}}}, "2026-10-12T12:00:00Z", false},
		{"invalid window is skipped, valid one still matches", &v1alpha1.MaintenanceSpec{Windows: []v1alpha1.MaintenanceWindow{{StartTime: "bad", EndTime: "02:00"}, {StartTime: "10:00", EndTime: "14:00"}}}, "2026-10-12T12:00:00Z", true},
		{"sunday night window, evening", &v1alpha1.MaintenanceSpec{Windows: []v1alpha1.MaintenanceWindow{night}}, "2026-10-11T23:30:00Z", true},
		{"sunday night window, monday tail", &v1alpha1.MaintenanceSpec{Windows: []v1alpha1.MaintenanceWindow{night}}, "2026-10-12T00:30:00Z", true},
		{"sunday night window, monday after end", &v1alpha1.MaintenanceSpec{Windows: []v1alpha1.MaintenanceWindow{night}}, "2026-10-12T01:00:00Z", false},
		{"any day matches", &v1alpha1.MaintenanceSpec{Windows: []v1alpha1.MaintenanceWindow{{Days: []string{"Any"}, StartTime: "10:00", EndTime: "14:00"}}}, "2026-10-14T12:00:00Z", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now, _ := time.Parse(time.RFC3339, tc.at)
			if got, _ := IsInMaintenanceWindow(tc.spec, now); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNextMaintenanceWindow(t *testing.T) {
	spec := &v1alpha1.MaintenanceSpec{
		ExcludeDates: []string{"2026-10-12"},
		Windows:      []v1alpha1.MaintenanceWindow{{Days: []string{"Mon", "Tue"}, StartTime: "22:00", EndTime: "02:00"}},
	}
	now, _ := time.Parse(time.RFC3339, "2026-10-12T08:00:00Z") // Monday, excluded
	got, err := NextMaintenanceWindow(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := time.Parse(time.RFC3339, "2026-10-13T22:00:00Z"); !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}

	if _, err := NextMaintenanceWindow(&v1alpha1.MaintenanceSpec{Windows: []v1alpha1.MaintenanceWindow{{Days: []string{"Mon"}, StartTime: "22:00", EndTime: "02:00"}}, ExcludeDates: []string{"2026-10-12", "2026-10-19"}}, now); err == nil {
		t.Fatal("want error when every candidate day is excluded")
	}
}

func TestNextMaintenanceWindowWithoutWindowsSkipsExcludedDates(t *testing.T) {
	spec := &v1alpha1.MaintenanceSpec{ExcludeDates: []string{"2026-10-12", "2026-10-13"}}
	now, _ := time.Parse(time.RFC3339, "2026-10-12T08:00:00Z")
	got, err := NextMaintenanceWindow(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := time.Parse(time.RFC3339, "2026-10-14T00:00:00Z"); !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestIsInWindowCrossesMidnight(t *testing.T) {
	w := v1alpha1.MaintenanceWindow{Days: []string{"Monday"}, StartTime: "22:00", EndTime: "02:00"}
	// 2026-10-12 is a Monday.
	tests := []struct {
		at   string
		want bool
	}{
		{"2026-10-12T21:59:00Z", false},
		{"2026-10-12T22:00:00Z", true},
		{"2026-10-13T01:00:00Z", true},  // Tuesday, tail of Monday's window
		{"2026-10-13T02:00:00Z", false}, // end exclusive
		{"2026-10-12T01:00:00Z", false}, // Monday morning is Sunday's window
		{"2026-10-13T22:30:00Z", false}, // Tuesday evening
	}
	for _, tc := range tests {
		now, _ := time.Parse(time.RFC3339, tc.at)
		got, err := isInWindow(w, now)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v (err %v), want %v", tc.at, got, err, tc.want)
		}
	}
}
