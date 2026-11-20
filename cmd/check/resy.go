package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	resyBase = "https://api.resy.com"
	// Public key embedded in resy.com's web bundle; RESY_API_KEY overrides it if it rotates.
	resyDefaultKey = "VbWk7s3L4KiK5fzlO7JD3Q5EYolJI7n5"
	browserUA      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
)

type resyProvider struct {
	http   *http.Client
	apiKey string
	cache  map[string]resyVenueInfo
	// release tallies on-sale rules per venue URL; see recordRelease.
	release map[string]map[string]int
}

func newResy() *resyProvider {
	key := os.Getenv("RESY_API_KEY")
	if key == "" {
		key = resyDefaultKey
	}
	return &resyProvider{http: &http.Client{Timeout: 15 * time.Second}, apiKey: key}
}

func (r *resyProvider) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resyBase+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	// api.resy.com answers an empty 500 unless Origin and Referer look like the web app.
	req.Header.Set("Authorization", fmt.Sprintf(`ResyAPI api_key="%s"`, r.apiKey))
	req.Header.Set("X-Origin", "https://resy.com")
	req.Header.Set("Origin", "https://resy.com")
	req.Header.Set("Referer", "https://resy.com/")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", browserUA)
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, path)
	}
	return b, nil
}

type resyVenueInfo struct {
	ID    string
	Notes []Note
}

// venueInfo resolves https://resy.com/cities/<location>/venues/<slug> to Resy's
// numeric ID and collects the booking-window sentences from the venue's own listing text.
func (r *resyProvider) venueInfo(ctx context.Context, raw string) (resyVenueInfo, error) {
	if info, ok := r.cache[raw]; ok {
		return info, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return resyVenueInfo{}, err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "cities" {
		return resyVenueInfo{}, fmt.Errorf("unexpected Resy URL %q", raw)
	}
	q := url.Values{}
	q.Set("url_slug", parts[len(parts)-1])
	q.Set("location", parts[1])
	b, err := r.get(ctx, "/3/venue", q)
	if err != nil {
		return resyVenueInfo{}, err
	}
	var v struct {
		ID struct {
			Resy int64 `json:"resy"`
		} `json:"id"`
		Content []struct {
			Name string `json:"name"`
			Body string `json:"body"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.ID.Resy == 0 {
		return resyVenueInfo{}, fmt.Errorf("no venue ID in response for %q", raw)
	}
	info := resyVenueInfo{ID: strconv.FormatInt(v.ID.Resy, 10)}
	seen := map[string]bool{}
	for _, c := range v.Content {
		if c.Name != "about" && c.Name != "need_to_know" && c.Name != "from_the_venue" {
			continue
		}
		for _, s := range splitSentences(c.Body) {
			if bookingWindow.MatchString(s) && !seen[s] {
				seen[s] = true
				info.Notes = append(info.Notes, Note{Text: s, Source: "Resy listing"})
			}
		}
	}
	if r.cache == nil {
		r.cache = map[string]resyVenueInfo{}
	}
	r.cache[raw] = info
	return info, nil
}

var bookingWindow = regexp.MustCompile(`(?i)\b(in advance|go(es)? live|released?|days? (out|ahead)|weeks? (out|ahead)|up to (\d+|one|two|three|four) (days?|weeks?|months?)|midnight|bookings? open|reservations? open)\b`)

// splitSentences splits on ., ! or ? followed by whitespace, and on line breaks.
func splitSentences(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		end := (s[i] == '.' || s[i] == '!' || s[i] == '?') && (i+1 == len(s) || s[i+1] == ' ' || s[i+1] == '\n')
		if end || s[i] == '\n' {
			if t := strings.TrimSpace(s[start : i+1]); t != "" {
				out = append(out, t)
			}
			start = i + 1
		}
	}
	if t := strings.TrimSpace(s[start:]); t != "" {
		out = append(out, t)
	}
	return out
}

// probeRelease looks for any slot at other party sizes so the on-sale rule can be learned
// for venues that have nothing open for the tracked party size. It scans every date for the
// first party size that has any slots, then stops.
func (r *resyProvider) probeRelease(ctx context.Context, v Venue, id string, dates []string) {
	for _, party := range []int{4, 3, 5, 6, 1} {
		for _, day := range dates {
			q := url.Values{}
			q.Set("lat", "0")
			q.Set("long", "0")
			q.Set("day", day)
			q.Set("party_size", strconv.Itoa(party))
			q.Set("venue_id", id)
			b, err := r.get(ctx, "/4/find", q)
			time.Sleep(300 * time.Millisecond)
			if err != nil {
				continue
			}
			var resp struct {
				Results struct {
					Venues []struct {
						Slots []struct {
							Market struct {
								Date struct {
									On int64 `json:"on"`
								} `json:"date"`
							} `json:"market"`
						} `json:"slots"`
					} `json:"venues"`
				} `json:"results"`
			}
			if json.Unmarshal(b, &resp) != nil {
				continue
			}
			for _, ven := range resp.Results.Venues {
				for _, s := range ven.Slots {
					r.recordRelease(v, day, s.Market.Date.On)
				}
			}
		}
		if len(r.release[v.URL]) > 0 {
			return
		}
	}
}

// recordRelease tallies one slot's on-sale moment as "<local HH:MM>|<days before the dining date>".
func (r *resyProvider) recordRelease(v Venue, day string, on int64) {
	if on == 0 {
		return
	}
	loc, err := time.LoadLocation(v.Timezone)
	if err != nil {
		return
	}
	d, err := time.ParseInLocation("2006-01-02", day, loc)
	if err != nil {
		return
	}
	onT := time.Unix(on, 0).In(loc)
	onDay := time.Date(onT.Year(), onT.Month(), onT.Day(), 0, 0, 0, 0, loc)
	lead := int(d.Sub(onDay).Round(24*time.Hour) / (24 * time.Hour))
	if r.release == nil {
		r.release = map[string]map[string]int{}
	}
	if r.release[v.URL] == nil {
		r.release[v.URL] = map[string]int{}
	}
	r.release[v.URL][fmt.Sprintf("%s|%d", onT.Format("15:04"), lead)]++
}

// ObservedRelease returns the most common on-sale rule seen across this run's slots.
func (r *resyProvider) ObservedRelease(v Venue) *Release {
	var bestKey string
	best, total := 0, 0
	for k, n := range r.release[v.URL] {
		total += n
		if n > best || (n == best && k < bestKey) {
			best, bestKey = n, k
		}
	}
	if best == 0 {
		return nil
	}
	parts := strings.Split(bestKey, "|")
	lead, _ := strconv.Atoi(parts[1])
	return &Release{Time: parts[0], LeadDays: lead, Timezone: v.Timezone, Samples: best}
}

func (r *resyProvider) ReleaseNotes(ctx context.Context, v Venue) ([]Note, error) {
	info, err := r.venueInfo(ctx, v.URL)
	return info.Notes, err
}

func (r *resyProvider) Availability(ctx context.Context, v Venue, dates []string) ([]DayAvailability, int, error) {
	info, err := r.venueInfo(ctx, v.URL)
	if err != nil {
		return nil, 0, err
	}
	id := info.ID
	var out []DayAvailability
	checked := 0
	var firstErr error
	for _, day := range dates {
		q := url.Values{}
		q.Set("lat", "0")
		q.Set("long", "0")
		q.Set("day", day)
		q.Set("party_size", strconv.Itoa(v.PartySize))
		q.Set("venue_id", id)
		b, err := r.get(ctx, "/4/find", q)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", day, err)
			}
			time.Sleep(time.Second)
			continue
		}
		var resp struct {
			Results struct {
				Venues []struct {
					Slots []struct {
						Date struct {
							Start string `json:"start"`
						} `json:"date"`
						Market struct {
							Date struct {
								On int64 `json:"on"`
							} `json:"date"`
						} `json:"market"`
					} `json:"slots"`
				} `json:"venues"`
			} `json:"results"`
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: decode: %w", day, err)
			}
			continue
		}
		checked++
		seen := map[string]bool{}
		var times []string
		for _, ven := range resp.Results.Venues {
			for _, s := range ven.Slots {
				r.recordRelease(v, day, s.Market.Date.On)
				// "2026-11-19 21:30:00" -> "21:30"
				if len(s.Date.Start) >= 16 && !seen[s.Date.Start[11:16]] {
					seen[s.Date.Start[11:16]] = true
					times = append(times, s.Date.Start[11:16])
				}
			}
		}
		if len(times) > 0 {
			sortTimes(times)
			out = append(out, DayAvailability{Date: day, Times: times})
		}
		time.Sleep(400 * time.Millisecond)
	}
	if v.NeedRelease && len(r.release[v.URL]) == 0 && ctx.Err() == nil {
		r.probeRelease(ctx, v, id, dates)
	}
	return out, checked, firstErr
}
