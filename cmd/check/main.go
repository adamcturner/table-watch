// Command check polls booking providers for each venue in venues.json and
// records the last time an open table was seen in docs/data/status.json.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"
)

type Venue struct {
	Name         string `json:"name"`
	City         string `json:"city"`
	Provider     string `json:"provider"`
	URL          string `json:"url"`
	Timezone     string `json:"timezone"`
	PartySize    int    `json:"party_size"`
	ReleaseNotes []Note `json:"release_notes,omitempty"` // hand-written notes for providers that expose none
}

// Note is one statement about when reservations open, with where it came from.
type Note struct {
	Text   string `json:"text"`
	Source string `json:"source"`
}

// ReleaseNoter is implemented by providers that can read a venue's booking-window policy.
type ReleaseNoter interface {
	ReleaseNotes(ctx context.Context, v Venue) ([]Note, error)
}

// DayAvailability lists the bookable start times (venue local, HH:MM) on one date.
type DayAvailability struct {
	Date  string   `json:"date"`
	Times []string `json:"times"`
}

type VenueStatus struct {
	Name          string            `json:"name"`
	City          string            `json:"city"`
	Provider      string            `json:"provider"`
	URL           string            `json:"url"`
	PartySize     int               `json:"party_size"`
	TrackingSince string            `json:"tracking_since"`
	LastChecked   string            `json:"last_checked"`
	CheckOK       bool              `json:"check_ok"`
	Error         string            `json:"error,omitempty"`
	DaysChecked   int               `json:"days_checked"`
	LastSeen      string            `json:"last_seen"`
	LastSeenSlots []DayAvailability `json:"last_seen_slots,omitempty"`
	AvailableNow  []DayAvailability `json:"available_now"`
	FurthestOpen  string            `json:"furthest_open,omitempty"`
	ReleaseNotes  []Note            `json:"release_notes,omitempty"`
}

type Status struct {
	GeneratedAt string        `json:"generated_at"`
	Venues      []VenueStatus `json:"venues"`
}

// Provider fetches availability for the given dates. It returns whatever it
// managed to read plus an error if any date could not be checked.
type Provider interface {
	Availability(ctx context.Context, v Venue, dates []string) ([]DayAvailability, int, error)
}

func providerFor(name string) (Provider, error) {
	switch name {
	case "resy":
		return newResy(), nil
	case "opentable":
		return newOpenTable(), nil
	}
	return nil, fmt.Errorf("unknown provider %q", name)
}

func main() {
	venuesPath := flag.String("venues", "venues.json", "venue list")
	outPath := flag.String("out", "docs/data/status.json", "status file to read and write")
	horizon := flag.Int("horizon", 30, "number of days ahead to check, starting today")
	flag.Parse()

	var venues []Venue
	if err := readJSON(*venuesPath, &venues); err != nil {
		log.Fatal(err)
	}
	var prev Status
	_ = readJSON(*outPath, &prev) // missing on first run
	prevByURL := map[string]VenueStatus{}
	for _, v := range prev.Venues {
		prevByURL[v.URL] = v
	}

	now := time.Now().UTC()
	ctx := context.Background()
	var next Status
	for _, v := range venues {
		old, seenBefore := prevByURL[v.URL]
		st := VenueStatus{
			Name: v.Name, City: v.City, Provider: v.Provider, URL: v.URL, PartySize: v.PartySize,
			TrackingSince: now.Format(time.RFC3339),
			LastChecked:   now.Format(time.RFC3339),
		}
		if seenBefore {
			st.TrackingSince = old.TrackingSince
			st.LastSeen = old.LastSeen
			st.LastSeenSlots = old.LastSeenSlots
			st.ReleaseNotes = old.ReleaseNotes
		}

		loc, err := time.LoadLocation(v.Timezone)
		if err != nil {
			log.Fatalf("%s: bad timezone %q", v.Name, v.Timezone)
		}
		today := now.In(loc)
		var dates []string
		for i := 0; i < *horizon; i++ {
			dates = append(dates, today.AddDate(0, 0, i).Format("2006-01-02"))
		}

		p, err := providerFor(v.Provider)
		if err != nil {
			st.Error = err.Error()
		} else {
			days, checked, err := p.Availability(ctx, v, dates)
			st.DaysChecked = checked
			st.AvailableNow = days
			if err != nil {
				st.Error = err.Error()
			} else {
				st.CheckOK = true
			}
		}
		if st.AvailableNow == nil {
			st.AvailableNow = []DayAvailability{}
		}
		if n := len(st.AvailableNow); n > 0 {
			st.FurthestOpen = st.AvailableNow[n-1].Date
		}
		if err == nil {
			if rn, ok := p.(ReleaseNoter); ok {
				if notes, nerr := rn.ReleaseNotes(ctx, v); nerr == nil && len(notes) > 0 {
					st.ReleaseNotes = notes
				}
			}
		}
		if len(v.ReleaseNotes) > 0 {
			st.ReleaseNotes = v.ReleaseNotes
		}
		if len(st.AvailableNow) > 0 {
			st.LastSeen = now.Format(time.RFC3339)
			st.LastSeenSlots = nil // available_now already holds them
		} else if seenBefore && len(old.AvailableNow) > 0 {
			st.LastSeenSlots = old.AvailableNow
		}
		log.Printf("%s: ok=%v checked=%d/%d open_dates=%d err=%q", v.Name, st.CheckOK, st.DaysChecked, len(dates), len(st.AvailableNow), st.Error)
		next.Venues = append(next.Venues, st)
	}
	next.GeneratedAt = now.Format(time.RFC3339)

	if !meaningfulChange(prev, next, now) {
		log.Println("no meaningful change; leaving status file untouched")
		return
	}
	if err := writeJSON(*outPath, next); err != nil {
		log.Fatal(err)
	}
	log.Println("wrote", *outPath)
}

// meaningfulChange avoids committing on every run: it ignores timestamps unless
// the previous check is more than three hours old.
func meaningfulChange(prev, next Status, now time.Time) bool {
	if len(prev.Venues) != len(next.Venues) {
		return true
	}
	for i := range next.Venues {
		a, b := prev.Venues[i], next.Venues[i]
		a.LastChecked, b.LastChecked = "", ""
		if !reflect.DeepEqual(a, b) {
			return true
		}
	}
	t, err := time.Parse(time.RFC3339, prev.GeneratedAt)
	return err != nil || now.Sub(t) > 3*time.Hour
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	if i := strings.LastIndex(path, "/"); i > 0 {
		if err := os.MkdirAll(path[:i], 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func sortTimes(t []string) { sort.Strings(t) }
