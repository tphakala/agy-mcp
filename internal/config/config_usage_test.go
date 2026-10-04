package config

import (
	"testing"
	"time"
)

func TestParseUsageInterval(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"empty is the default", "", DefaultUsageInterval, false},
		{"zero disables", "0", 0, false},
		{"minutes", "10m", 10 * time.Minute, false},
		{"exactly one minute", "60s", time.Minute, false},
		{"negative", "-1m", 0, true},
		{"under a minute", "30s", 0, true},
		{"garbage", "soon", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseUsageInterval(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseUsageInterval(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("parseUsageInterval(%q) = %s, want %s", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParsePercent(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		def     float64
		want    float64
		wantErr bool
	}{
		{"empty is the default", "", 0.25, 0.25, false},
		{"plain", "15", 0, 0.15, false},
		{"with percent sign", "15%", 0, 0.15, false},
		{"zero", "0", 0.25, 0, false},
		{"hundred", "100", 0, 1, false},
		{"over hundred", "101", 0, 0, true},
		{"negative", "-1", 0, 0, true},
		{"garbage", "abc", 0, 0, true},
		{"nan", "NaN", 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePercent("AGY_MCP_USAGE_LOW", tt.raw, tt.def)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePercent(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
			if err == nil && (got < tt.want-1e-9 || got > tt.want+1e-9) {
				t.Fatalf("parsePercent(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
