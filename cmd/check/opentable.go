package main

import (
	"context"
	"errors"
)

type openTableProvider struct{}

func newOpenTable() *openTableProvider { return &openTableProvider{} }

func (o *openTableProvider) Availability(ctx context.Context, v Venue, dates []string) ([]DayAvailability, int, error) {
	return nil, 0, errors.New("OpenTable checking is not implemented yet")
}
