# table-watch

A static site that lists restaurants being tracked and the last time an open table was seen at each.

A GitHub Actions job runs every 30 minutes, scans up to 30 days ahead for each venue, and writes the result to `docs/data/status.json`. GitHub Pages serves `docs/`.

## Add a restaurant

Add an entry to `venues.json`:

    {
      "name": "Restaurant Name",
      "city": "City, ST",
      "provider": "resy",
      "url": "https://resy.com/cities/<city>/venues/<slug>",
      "timezone": "America/New_York",
      "party_size": 2
    }

Pushing the change triggers a check immediately.

## Providers

- `resy`: working. Uses the same public search call as resy.com and needs no account.
- `opentable`: listed, not yet checkable. OpenTable blocks plain HTTP clients, so the site shows these venues as "Not checkable yet".

## Run locally

    go run ./cmd/check -out /tmp/status.json
