package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// maxReportDataBytes bounds the aggregate of one inspection; 50 rooms with checklists and
// defects stay far below it.
const maxReportDataBytes = 8 << 20

// ErrNotFound means core-service does not know the inspection.
var ErrNotFound = errors.New("inspection not found")

// ReportData is the part of core.yaml ReportData that the report shows.
type ReportData struct {
	Property    Property    `json:"property"`
	Inspection  Inspection  `json:"inspection"`
	Inspector   Inspector   `json:"inspector"`
	Rooms       []Room      `json:"rooms"`
	Summary     Summary     `json:"summary"`
	Comparison  *Comparison `json:"comparison"`
	GeneratedAt time.Time   `json:"generatedAt"`
}

// Property of the inspection.
type Property struct {
	ID      uuid.UUID `json:"id"`
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Address string    `json:"address"`
	City    string    `json:"city"`
}

// Inspection being reported.
type Inspection struct {
	ID          uuid.UUID `json:"id"`
	Title       string    `json:"title"`
	InspectedAt time.Time `json:"inspectedAt"`
	Status      string    `json:"status"`
	CompletedAt time.Time `json:"completedAt"`
	Notes       string    `json:"notes"`
}

// Inspector who conducted the inspection.
type Inspector struct {
	FullName string `json:"fullName"`
}

// Room with its checklist and the defects observed in it.
type Room struct {
	Room      RoomInfo        `json:"room"`
	Checklist []ChecklistItem `json:"checklist"`
	Defects   []RoomDefect    `json:"defects"`
}

// RoomInfo names a room.
type RoomInfo struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	RoomType string    `json:"roomType"`
}

// ChecklistItem with its result.
type ChecklistItem struct {
	Title        string `json:"title"`
	ElementGroup string `json:"elementGroup"`
	Result       string `json:"result"`
	Comment      string `json:"comment"`
}

// RoomDefect is a defect with its observation in this inspection.
type RoomDefect struct {
	Defect       Defect      `json:"defect"`
	CategoryName string      `json:"categoryName"`
	Observation  Observation `json:"observation"`
}

// Defect of the property.
type Defect struct {
	ID           uuid.UUID `json:"id"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	ElementGroup string    `json:"elementGroup"`
}

// Observation of a defect in the inspection.
type Observation struct {
	Status   string  `json:"status"`
	Severity string  `json:"severity"`
	Comment  string  `json:"comment"`
	Photos   []Photo `json:"photos"`
}

// Photo attached to an observation.
type Photo struct {
	PhotoID uuid.UUID `json:"photoId"`
	Caption string    `json:"caption"`
}

// Summary of the defects found open in the inspection.
type Summary struct {
	Total      int            `json:"total"`
	BySeverity map[string]int `json:"bySeverity"`
}

// Comparison with the previous inspection (ADR-004).
type Comparison struct {
	Base    ComparedInspection `json:"base"`
	Summary StateCounts        `json:"summary"`
	Defects []ComparedDefect   `json:"defects"`
}

// ComparedInspection is the previous inspection.
type ComparedInspection struct {
	ID          uuid.UUID `json:"id"`
	InspectedAt time.Time `json:"inspectedAt"`
}

// StateCounts counts defects in each comparison state.
type StateCounts struct {
	New        int `json:"new"`
	Resolved   int `json:"resolved"`
	Remaining  int `json:"remaining"`
	NotChecked int `json:"notChecked"`
}

// ComparedDefect is one defect of the comparison.
type ComparedDefect struct {
	Title    string `json:"title"`
	State    string `json:"state"`
	Reopened bool   `json:"reopened"`
	Severity string `json:"severity"`
	RoomName string `json:"roomName"`
}

// ReportData returns the aggregate of the inspection in the given language (ru or en).
func (c *Client) ReportData(ctx context.Context, inspectionID uuid.UUID, locale string) (ReportData, error) {
	endpoint := c.base + "/internal/v1/inspections/" + inspectionID.String() + "/report-data?" +
		url.Values{"locale": {locale}}.Encode()
	resp, err := c.authorizedGet(ctx, endpoint)
	if err != nil {
		return ReportData{}, err
	}
	defer closeBody(resp)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return ReportData{}, ErrNotFound
	default:
		return ReportData{}, fmt.Errorf("%w: report data answered %d", ErrUnavailable, resp.StatusCode)
	}
	var data ReportData
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReportDataBytes)).Decode(&data); err != nil {
		return ReportData{}, fmt.Errorf("%w: unreadable report data: %w", ErrUnavailable, err)
	}
	return data, nil
}

// authorizedGet sends a GET with the service token; a rejected token is replaced once, as after
// a restart of core-service.
func (c *Client) authorizedGet(ctx context.Context, endpoint string) (*http.Response, error) {
	resp, err := c.getWithToken(ctx, endpoint)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		closeBody(resp)
		c.dropToken()
		resp, err = c.getWithToken(ctx, endpoint)
	}
	return resp, err
}

func (c *Client) getWithToken(ctx context.Context, endpoint string) (*http.Response, error) {
	token, err := c.ServiceToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return resp, nil
}
