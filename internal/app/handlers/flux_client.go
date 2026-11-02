package handlers

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// fluxClient is a minimal InfluxDB v2 query-API client, hand-rolled so the
// proxy does not gain a heavyweight client dependency for one POST endpoint.
// The org travels as a URL query parameter deliberately: deployments that
// reach Influx through a host-published port reject bodies whose "org" is a
// name ("provide orgID or org"), while the query parameter resolves on every
// transport tried - and this client is only ever pointed at the
// docker-internal URL anyway.
type fluxClient struct {
	client  *http.Client
	baseURL string
	org     string
	token   string
}

// fluxClientTimeout bounds one Flux round trip: the metrics provider runs
// every 10s, so a wedged Influx must surface as an error, never as a stuck
// refresh loop (the write-path stall of 2026-10-12 is the cautionary tale).
const fluxClientTimeout = 5 * time.Second

func newFluxClient(baseURL, org, token string) *fluxClient {
	return &fluxClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		org:     org,
		token:   token,
		client:  &http.Client{Timeout: fluxClientTimeout},
	}
}

// query runs one Flux script and returns rows as column->value maps.
//
// The wire format is annotated CSV, deliberately: InfluxDB accepts a
// "dialect" for /api/v2/query, but deployments routinely run with server
// defaults that IGNORE a json dialect (verified against the live influxd:
// format:"json" still streams CSV) - parsing what the server actually sends
// beats trusting a capability it may not honour. The stream is one header
// row (",result,table,...") plus data rows ("_result" in the result column)
// per result table; an "error" table signals a Flux-level failure after an
// HTTP 200. Values arrive as strings; the influxFloat/influxTime helpers at
// the call sites coerce them.
func (c *fluxClient) query(ctx context.Context, flux string) ([]map[string]any, error) {
	payload, err := json.Marshal(map[string]any{
		"query":   flux,
		"type":    "flux",
		"dialect": map[string]any{"format": "csv", "header": true},
	})
	if err != nil {
		return nil, err
	}
	endpoint := c.baseURL + "/api/v2/query?org=" + url.QueryEscape(c.org)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Token "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("influx query failed: %s: %s", resp.Status, strings.TrimSpace(string(excerpt)))
	}
	var rows []map[string]any
	reader := csv.NewReader(resp.Body)
	reader.FieldsPerRecord = -1
	reader.Comment = '#'
	var columns []string
	inErrorTable := false
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("influx query response malformed: %w", err)
		}
		// Every annotation-column row starts with the empty annotation
		// cell; anything else is not part of a result table.
		if len(record) < 2 || record[0] != "" {
			continue
		}
		switch record[1] {
		case "result":
			columns = record
			inErrorTable = false
			continue
		case "error":
			columns = record
			inErrorTable = true
			continue
		}
		if inErrorTable {
			return nil, fmt.Errorf("influx query error: %s", strings.TrimSpace(strings.Join(record[1:], " ")))
		}
		if len(columns) == 0 {
			continue
		}
		row := make(map[string]any, len(columns))
		for i, name := range columns {
			if name == "" || i >= len(record) {
				continue
			}
			row[name] = record[i]
		}
		rows = append(rows, row)
	}
	return rows, nil
}
