package questions

import (
	"context"
	"testing"

	"agentfleet/internal/zulip"
)

// fakeNotifier records every call — table-test-friendly without a real
// bridge or Zulip Cloud.
type fakeNotifier struct {
	calls []zulip.NotifyRequest
	err   error
}

func (f *fakeNotifier) Notify(_ context.Context, req zulip.NotifyRequest) error {
	if f.err != nil {
		return f.err
	}

	f.calls = append(f.calls, req)

	return nil
}

// TestConfigDefaults proves DefaultConfig's own literal ladder
// (development-plan.md §6: "nudge at 4h, escalate at 24h, park ... at
// 72h") is what an unconfigured Sweeper actually uses.
func TestConfigDefaults(t *testing.T) {
	s := &Sweeper{}

	cfg := s.config()

	def := DefaultConfig()
	if cfg != def {
		t.Fatalf("config() = %+v, want DefaultConfig() = %+v", cfg, def)
	}
}

// TestConfigOverridesIndividualFields proves a caller-set field survives
// and an unset one still falls back — not "any override replaces the whole
// struct."
func TestConfigOverridesIndividualFields(t *testing.T) {
	s := &Sweeper{Config: Config{Nudge: 1}}

	cfg := s.config()

	if cfg.Nudge != 1 {
		t.Fatalf("Nudge = %v, want the caller's override (1)", cfg.Nudge)
	}

	if cfg.Escalate != DefaultConfig().Escalate {
		t.Fatalf("Escalate = %v, want the default (unset field)", cfg.Escalate)
	}
}
