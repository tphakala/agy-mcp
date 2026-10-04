package mcptools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

// defaultUsageMaxAge is how old a reading agy_usage accepts when max_age is
// omitted. It matches the manager's cache TTL.
const defaultUsageMaxAge = 60 * time.Second

type quotaInput struct {
	MaxAge string `json:"max_age,omitempty" jsonschema:"how old a cached reading may be (Go duration, e.g. 10m); default 60s, and anything under 10s is raised to 10s. Use a long value (10m) to read the background snapshot instantly; use 10s to accept only a reading under 10 seconds old; a new reading takes a few seconds because it runs agy"`
}

type quotaBucketOutput struct {
	ID                string  `json:"id" jsonschema:"agy's identifier for this quota window, e.g. 5h or weekly"`
	Name              string  `json:"name" jsonschema:"human-readable name of the window"`
	Window            string  `json:"window" jsonschema:"the window length as agy reports it, e.g. 5h or weekly"`
	RemainingFraction float64 `json:"remaining_fraction" jsonschema:"remaining quota from 0 to 1 as agy last reported it; not adjusted for a reset that has passed since, see refilled"`
	ResetTime         string  `json:"reset_time,omitempty" jsonschema:"when this window resets, RFC3339 UTC; absent when agy reported none"`
	Refilled          bool    `json:"refilled" jsonschema:"true when reset_time has passed since the reading, so this window counts as full for level and remaining_percent"`
}

type quotaGroupOutput struct {
	Name             string              `json:"name" jsonschema:"quota group name, e.g. Gemini Models; models in a group share its limits"`
	Description      string              `json:"description,omitempty" jsonschema:"what agy says the group covers"`
	Level            string              `json:"level" jsonschema:"ok, low, critical or exhausted, set by the tightest window of the group at the time of the call; see the tool description for the thresholds"`
	RemainingPercent int                 `json:"remaining_percent" jsonschema:"remaining quota of the tightest window, as a whole percent rounded down; level is the authority, so 0.4 percent reads 0 with level critical"`
	Window           string              `json:"window" jsonschema:"the tightest window, the one that sets level"`
	ResetTime        string              `json:"reset_time,omitempty" jsonschema:"when the tightest window resets, RFC3339 UTC; absent when agy reported none"`
	Buckets          []quotaBucketOutput `json:"buckets" jsonschema:"every window of the group"`
}

type quotaOutput struct {
	CheckedAt string             `json:"checked_at" jsonschema:"when agy was asked for this reading, RFC3339 UTC"`
	Groups    []quotaGroupOutput `json:"groups" jsonschema:"one entry per quota group; empty when agy reported none"`
}

type quotaGroupSummary struct {
	Name             string `json:"name" jsonschema:"quota group name, e.g. Gemini Models"`
	Level            string `json:"level" jsonschema:"ok, low, critical or exhausted"`
	RemainingPercent int    `json:"remaining_percent" jsonschema:"remaining quota of the tightest window, whole percent rounded down"`
	Window           string `json:"window" jsonschema:"the tightest window, the one that sets level"`
	ResetTime        string `json:"reset_time,omitempty" jsonschema:"when the tightest window resets, RFC3339 UTC"`
}

// quotaSummaryOutput is the compact quota reading attached to run results. It is
// read from the server's in-memory snapshot, never by running agy, so it can be
// absent.
type quotaSummaryOutput struct {
	CheckedAt string              `json:"checked_at" jsonschema:"when agy was asked for this reading, RFC3339 UTC"`
	Groups    []quotaGroupSummary `json:"groups" jsonschema:"one entry per quota group, with the level the server computed now"`
}

func formatQuotaTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// toQuotaOutput converts a snapshot and its evaluated groups (same order, see
// manager.Manager.QuotaLevels) into the agy_usage wire shape. Arrays are never
// null.
func toQuotaOutput(snap manager.QuotaSnapshot, levels []manager.GroupQuota) quotaOutput {
	out := quotaOutput{CheckedAt: formatQuotaTime(snap.CheckedAt), Groups: make([]quotaGroupOutput, 0, len(levels))}
	now := time.Now()
	for i := range levels {
		gq := &levels[i]
		g := quotaGroupOutput{
			Name:             gq.Group.Name,
			Description:      gq.Group.Description,
			Level:            string(gq.Level),
			RemainingPercent: gq.Percent(),
			Window:           gq.Binding.Window,
			ResetTime:        formatQuotaTime(gq.Binding.ResetTime),
			Buckets:          make([]quotaBucketOutput, 0, len(gq.Group.Buckets)),
		}
		for _, b := range gq.Group.Buckets {
			g.Buckets = append(g.Buckets, quotaBucketOutput{
				ID: b.ID, Name: b.Name, Window: b.Window,
				RemainingFraction: b.RemainingFraction,
				ResetTime:         formatQuotaTime(b.ResetTime),
				Refilled:          !b.ResetTime.IsZero() && !b.ResetTime.After(now),
			})
		}
		out.Groups = append(out.Groups, g)
	}
	return out
}

// toQuotaSummary is the compact form of toQuotaOutput.
func toQuotaSummary(snap manager.QuotaSnapshot, levels []manager.GroupQuota) *quotaSummaryOutput {
	out := &quotaSummaryOutput{CheckedAt: formatQuotaTime(snap.CheckedAt), Groups: make([]quotaGroupSummary, 0, len(levels))}
	for i := range levels {
		gq := &levels[i]
		out.Groups = append(out.Groups, quotaGroupSummary{
			Name:             gq.Group.Name,
			Level:            string(gq.Level),
			RemainingPercent: gq.Percent(),
			Window:           gq.Binding.Window,
			ResetTime:        formatQuotaTime(gq.Binding.ResetTime),
		})
	}
	return out
}

// quotaFor returns the quota summary to attach to a run result, or nil when the
// server has no trustworthy reading. It never waits on agy.
func quotaFor(mgr *manager.Manager) *quotaSummaryOutput {
	snap, ok := mgr.CachedQuota()
	if !ok {
		return nil
	}
	return toQuotaSummary(snap, mgr.QuotaLevels(snap))
}

// registerUsage adds the agy_usage tool.
func registerUsage(s *mcp.Server, mgr *manager.Manager) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        toolAgyUsage,
		Title:       "Check agy quota",
		Annotations: annReadExternal,
		Description: "Report how much agy quota is left, per quota group (for example Gemini Models, shared by every Gemini model, Flash and Pro alike), with a level per group: ok, low, critical or exhausted. " +
			"The level comes from the group's tightest window (5h or weekly), counts a window whose reset time has passed as full, and uses the server's thresholds: low below 25 percent remaining, critical below 5, exhausted at 0 (AGY_MCP_USAGE_LOW and AGY_MCP_USAGE_CRITICAL change the first two). " +
			"It spends no model quota: it runs agy's /usage command, which starts no agent turn. " +
			"Check it before optional work, and pass priority optional to agy_run or agy_run_sync so the server refuses the run while the model's group is low; the same reading rides on the quota field of run, status and wait results. " +
			"max_age bounds how old a cached reading may be (default 60s, minimum 10s); a fresh reading takes a few seconds. " +
			"Shells out to the agy CLI, so it fails if agy is missing from PATH or not authenticated.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in quotaInput) (*mcp.CallToolResult, quotaOutput, error) {
		maxAge := defaultUsageMaxAge
		if in.MaxAge != "" {
			d, err := time.ParseDuration(in.MaxAge)
			if err != nil {
				return nil, quotaOutput{}, fmt.Errorf("invalid max_age %q: %w", in.MaxAge, err)
			}
			if d < 0 {
				return nil, quotaOutput{}, fmt.Errorf("invalid max_age %q: want a non-negative Go duration like 10m", in.MaxAge)
			}
			maxAge = d
		}
		snap, err := mgr.Usage(ctx, maxAge)
		if err != nil {
			return nil, quotaOutput{}, err
		}
		return nil, toQuotaOutput(snap, mgr.QuotaLevels(snap)), nil
	})
}
