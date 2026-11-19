package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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

// venueID resolves https://resy.com/cities/<location>/venues/<slug> to Resy's numeric ID.
func (r *resyProvider) venueID(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "cities" {
		return "", fmt.Errorf("unexpected Resy URL %q", raw)
	}
	q := url.Values{}
	q.Set("url_slug", parts[len(parts)-1])
	q.Set("location", parts[1])
	b, err := r.get(ctx, "/3/venue", q)
	if err != nil {
		return "", err
	}
	var v struct {
		ID struct {
			Resy int64 `json:"resy"`
		} `json:"id"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.ID.Resy == 0 {
		return "", fmt.Errorf("no venue ID in response for %q", raw)
	}
	return strconv.FormatInt(v.ID.Resy, 10), nil
}

func (r *resyProvider) Availability(ctx context.Context, v Venue, dates []string) ([]DayAvailability, int, error) {
	id, err := r.venueID(ctx, v.URL)
	if err != nil {
		return nil, 0, err
	}
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
	return out, checked, firstErr
}
